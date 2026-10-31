package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

// entityFlags are the descriptive fields shared by workflow and task create/update.
type entityFlags struct {
	name        string
	id          string
	labels      []string
	annotations []string
	assignees   []string
}

func (e *entityFlags) bind(cmd *cobra.Command, create bool) {
	f := cmd.Flags()
	f.StringVar(&e.name, "name", "", "display name")
	if create {
		f.StringVar(&e.id, "id", "", "reserved: client-assigned ids are rejected with CLIENT_ID_UNSUPPORTED; ids are server-assigned")
	}
	f.StringArrayVar(&e.labels, "label", nil, "label key=value (repeatable, filterable)")
	f.StringArrayVar(&e.annotations, "annotation", nil, "annotation key=value (repeatable, free text)")
	f.StringArrayVar(&e.assignees, "assignee", nil, "role=account, e.g. dev_owner=alice (repeatable)")
}

func (e *entityFlags) maps() (labels, ann, asg map[string]string, err error) {
	if labels, err = parseKV(e.labels, "label"); err != nil {
		return
	}
	if ann, err = parseKV(e.annotations, "annotation"); err != nil {
		return
	}
	asg, err = parseAssignees(e.assignees)
	return
}

// mask builds the update body fragment and update_mask from the provided flags.
func (e *entityFlags) updateFields(cmd *cobra.Command) (map[string]any, string, error) {
	labels, ann, asg, err := e.maps()
	if err != nil {
		return nil, "", err
	}
	body := map[string]any{}
	var mask []string
	if cmd.Flags().Changed("name") {
		body["display_name"] = e.name
		mask = append(mask, "display_name")
	}
	if labels != nil {
		body["labels"] = labels
		mask = append(mask, "labels")
	}
	if ann != nil {
		body["annotations"] = ann
		mask = append(mask, "annotations")
	}
	if asg != nil {
		body["assignees"] = asg
		mask = append(mask, "assignees")
	}
	if len(mask) == 0 {
		return nil, "", usagef("nothing to update: pass --name, --label, --annotation or --assignee")
	}
	return body, strings.Join(mask, ","), nil
}

func (a *App) workflowCmd() *cobra.Command {
	return group("workflow", "Create, read, update, delete and restore workflows",
		a.workflowCreate(), a.workflowGet(), a.workflowList(), a.workflowUpdate(),
		a.workflowDelete(), a.workflowRestore())
}

func (a *App) workflowCreate() *cobra.Command {
	var e entityFlags
	var w writeFlags
	cmd := &cobra.Command{
		Use: "create --name NAME [--id ID] [--label k=v] [--assignee role=account]", Short: "Create a workflow",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if e.name == "" {
				return usagef("--name is required")
			}
			labels, ann, asg, err := e.maps()
			if err != nil {
				return err
			}
			if err := w.noBody("create"); err != nil {
				return err
			}
			body := api.Workflow{DisplayName: &e.name}
			if labels != nil {
				l := api.Labels(labels)
				body.Labels = &l
			}
			if ann != nil {
				x := api.Annotations(ann)
				body.Annotations = &x
			}
			if asg != nil {
				x := api.Assignees(asg)
				body.Assignees = &x
			}
			path, err := a.collectionPath("workflows")
			if err != nil {
				return err
			}
			q, err := w.query(false)
			if err != nil {
				return err
			}
			if e.id != "" {
				q.Set("workflow_id", strings.ToLower(e.id))
			}
			out, err := a.call("POST", path, q, body)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	e.bind(cmd, true)
	w.bind(cmd)
	return cmd
}

func (a *App) workflowGet() *cobra.Command {
	return &cobra.Command{
		Use: "get WF", Short: "Get a workflow (WF-000123, wf-000123 or full resource name)",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			out, err := a.call("GET", path, nil, nil)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
}

func (a *App) workflowList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use: "list [--filter EXPR] [--page-count N]", Short: "List workflows",
		Args: exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := a.collectionPath("workflows")
			if err != nil {
				return err
			}
			return a.listCall(path, &l, "")
		},
	}
	l.bind(cmd, true)
	return cmd
}

type updateBody struct {
	api.MutationOptions
	Workflow map[string]any `json:"workflow,omitempty"`
	Task     map[string]any `json:"task,omitempty"`
}

func (a *App) workflowUpdate() *cobra.Command {
	var e entityFlags
	var w writeFlags
	cmd := &cobra.Command{
		Use: "update WF [--name N] [--label k=v] [--annotation k=v] [--assignee role=account]", Short: "Update a workflow (each given field is replaced as a whole)",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			fields, mask, err := e.updateFields(cmd)
			if err != nil {
				return err
			}
			o, err := w.options()
			if err != nil {
				return err
			}
			out, err := a.call("PATCH", path, map[string][]string{"update_mask": {mask}}, updateBody{MutationOptions: o, Workflow: fields})
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	e.bind(cmd, false)
	w.bind(cmd)
	return cmd
}

func (a *App) workflowDelete() *cobra.Command {
	var w writeFlags
	cmd := &cobra.Command{
		Use: "delete WF", Short: "Soft-delete a workflow", Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			q, err := w.query(true)
			if err != nil {
				return err
			}
			out, err := a.call("DELETE", path, q, nil)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	w.bind(cmd)
	return cmd
}

func (a *App) workflowRestore() *cobra.Command {
	var w writeFlags
	cmd := &cobra.Command{
		Use: "restore WF", Short: "Restore a soft-deleted workflow", Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			o, err := w.options()
			if err != nil {
				return err
			}
			out, err := a.call("POST", path+":undelete", nil, o)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	w.bind(cmd)
	return cmd
}
