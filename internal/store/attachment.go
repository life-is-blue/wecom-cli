package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

// Attachment entities carry the keys: on (workflow id), filename, size, sha256, storage_key.
// The bytes live in internal/storage; the event stream records who registered them.

func (p Project) attachmentName(wf, id int64) string {
	return p.workflowName(wf) + "/attachments/" + FormatID(PrefixAttach, id)
}

// AttachmentName is the exported resource-name helper.
func (p Project) AttachmentName(wf, id int64) string { return p.attachmentName(wf, id) }

// StorageKeyPrefix is the key prefix every blob of a workflow must have.
func (p Project) StorageKeyPrefix(wf int64) string { return fmt.Sprintf("%d/%d/", p.ID, wf) }

// ParseAttachmentRef accepts a full attachment resource name or a bare id (at-000009) and
// returns the attachment entity id. A full name must address the given workflow of this project.
func (p Project) ParseAttachmentRef(wf int64, ref string) (int64, bool) {
	ref = strings.TrimSpace(ref)
	if rest, ok := strings.CutPrefix(strings.ToLower(ref), strings.ToLower(p.workflowName(wf))+"/attachments/"); ok {
		return ParseID(PrefixAttach, rest)
	}
	if strings.Contains(ref, "/") {
		return 0, false
	}
	return ParseID(PrefixAttach, ref)
}

// RenderAttachment maps folded state to the API Attachment.
func RenderAttachment(p Project, wf int64, e *Entity) api.Attachment {
	id := strconv.FormatInt(e.ID, 10)
	size, _ := strconv.ParseInt(core.Scalar(e.Vals["size"]), 10, 64)
	a := api.Attachment{
		Filename:   e.str("filename"),
		Size:       size,
		Sha256:     e.str("sha256"),
		StorageKey: e.str("storage_key"),
		CreateTime: ptr(e.CreateTime),
	}
	if e.ID != 0 {
		a.Name = ptr(p.attachmentName(wf, e.ID))
		a.Uid = &id
	}
	if e.Creator != "" {
		a.Creator = ptr(accountName(e.Creator))
	}
	return a
}

// PreviewAttachment renders a validate_only result.
func (s *Store) PreviewAttachment(p Project, wf int64, e *Entity) *api.Attachment {
	a := RenderAttachment(p, wf, e)
	return &a
}

// GetAttachment reads one attachment of a workflow.
func (s *Store) GetAttachment(ctx context.Context, p Project, wf, id int64) (*api.Attachment, error) {
	e, err := loadEntity(ctx, s.DB, p.ID, "attachment", id)
	if err != nil {
		return nil, internalErr(notFoundAs(err, "attachment"))
	}
	if core.Scalar(e.Vals["on"]) != strconv.FormatInt(wf, 10) {
		return nil, core.NewError(core.StatusNotFound, "NOT_FOUND", "attachment not found")
	}
	a := RenderAttachment(p, wf, e)
	return &a, nil
}

func notFoundAs(err error, what string) error {
	var ce *core.Error
	if asCore(err, &ce) && ce.Reason == "NOT_FOUND" {
		return core.NewError(core.StatusNotFound, "NOT_FOUND", "%s not found", what)
	}
	return err
}

// StorageKeyRegistered reports whether an attachment of the project already uses the storage key.
func (s *Store) StorageKeyRegistered(ctx context.Context, p Project, key string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity_latest l JOIN entity e ON e.id = l.entity_id
		WHERE l.k = 'storage_key' AND e.project_id = ? AND e.type = 'attachment' AND JSON_UNQUOTE(l.v) = ?`, p.ID, key).Scan(&n)
	return n > 0, internalErr(err)
}

// ResolveEvidence validates evidence references of a workflow and returns the canonical resource
// names (deduplicated, in the given order). Every reference must be an existing attachment of
// this very workflow.
func (s *Store) ResolveEvidence(ctx context.Context, p Project, wf int64, refs []string) ([]string, error) {
	var ids []int64
	seen := map[int64]bool{}
	for _, r := range refs {
		id, ok := p.ParseAttachmentRef(wf, r)
		if !ok {
			return nil, core.NewError(core.StatusInvalidArgument, "INVALID_EVIDENCE", "evidence %q is not an attachment of this workflow", r)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	ents, err := loadEntities(ctx, s.DB, p.ID, "attachment", ids, time.Time{})
	if err != nil {
		return nil, internalErr(err)
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		e := ents[id]
		if e == nil || core.Scalar(e.Vals["on"]) != strconv.FormatInt(wf, 10) {
			return nil, core.NewError(core.StatusInvalidArgument, "INVALID_EVIDENCE", "evidence attachment %s does not exist in this workflow", FormatID(PrefixAttach, id))
		}
		names = append(names, p.attachmentName(wf, id))
	}
	return names, nil
}

// StorageKeyRegisteredBy reports whether an attachment of the project uses the storage key and, when
// requestID is non-empty, whether that attachment was created by a request with this idempotency key
// (a replay). Without a request_id the second result is always false.
func (s *Store) StorageKeyRegisteredBy(ctx context.Context, p Project, key, requestID string) (registered, byRequest bool, err error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT e.id FROM entity_latest l JOIN entity e ON e.id = l.entity_id
		WHERE l.k = 'storage_key' AND e.project_id = ? AND e.type = 'attachment' AND JSON_UNQUOTE(l.v) = ?`, p.ID, key)
	if err != nil {
		return false, false, internalErr(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, false, internalErr(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return false, false, internalErr(err)
	}
	if requestID == "" {
		return true, false, nil
	}
	for _, id := range ids {
		var n int
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM event WHERE project_id = ? AND entity_id = ? AND idempotency_key = ?`,
			p.ID, id, requestID).Scan(&n); err != nil {
			return true, false, internalErr(err)
		}
		if n > 0 {
			return true, true, nil
		}
	}
	return true, false, nil
}
