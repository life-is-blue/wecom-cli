package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

func (a *App) stageCmd() *cobra.Command {
	return group("stage", "Advance or regress workflow stages", a.stageAdvance(), a.stageRegress())
}

func (a *App) stageAdvance() *cobra.Command {
	var w writeFlags
	var stage string
	var evidence []string
	cmd := &cobra.Command{
		Use: "advance WF --stage STAGE", Short: "Mark a stage PASSED",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if stage == "" {
				return usagef("--stage is required")
			}
			path, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			o, err := w.options()
			if err != nil {
				return err
			}
			body := api.AdvanceStageRequest{Stage: strings.ToLower(stage),
				Etag: o.Etag, Note: o.Note, Reason: o.Reason, RequestId: o.RequestId, ValidateOnly: o.ValidateOnly}
			if len(evidence) > 0 {
				body.EvidenceAttachments = &evidence
			}
			out, err := a.call("POST", path+":advanceStage", nil, body)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	cmd.Flags().StringVar(&stage, "stage", "", "stage key, e.g. development")
	bindEvidence(cmd, &evidence)
	w.bind(cmd)
	return cmd
}

// bindEvidence adds --evidence: attachments of the workflow that prove a change. An admin who acts for
// the other side (a stage or scope change the rule table reserves for it) must pass them.
func bindEvidence(cmd *cobra.Command, v *[]string) {
	cmd.Flags().StringArrayVar(v, "evidence", nil, "attachment resource name as evidence (repeatable); required when an admin acts for the other side")
}

func (a *App) stageRegress() *cobra.Command {
	var w writeFlags
	var stages []string
	var evidence []string
	cmd := &cobra.Command{
		Use: "regress WF --stage STAGE [--stage STAGE] --reason REJECT_REASON", Short: "Set PASSED stages back to OPEN with a reject reason",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(stages) == 0 {
				return usagef("at least one --stage is required")
			}
			if w.reason == "" {
				return usagef("--reason is required (a reject-kind reason_key, e.g. rej_row_count)")
			}
			path, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			o, err := w.options()
			if err != nil {
				return err
			}
			for i := range stages {
				stages[i] = strings.ToLower(stages[i])
			}
			body := api.RegressStageRequest{Stages: stages, Reason: w.reason,
				Etag: o.Etag, Note: o.Note, RequestId: o.RequestId, ValidateOnly: o.ValidateOnly}
			if len(evidence) > 0 {
				body.EvidenceAttachments = &evidence
			}
			out, err := a.call("POST", path+":regressStage", nil, body)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	cmd.Flags().StringSliceVar(&stages, "stage", nil, "stage to set back to OPEN (repeatable); name every PASSED stage after it too")
	bindEvidence(cmd, &evidence)
	w.bind(cmd)
	return cmd
}

func (a *App) scopeCmd() *cobra.Command {
	return group("scope", "Move workflows in or out of migration scope", a.scopeSet())
}

func (a *App) scopeSet() *cobra.Command {
	var w writeFlags
	var to, into string
	var evidence []string
	cmd := &cobra.Command{
		Use: "set WF --to in|out [--reason R] [--into WF]", Short: "Set scope; out needs a discard reason, `--reason merged` needs --into",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var scope api.Scope
			switch strings.ToLower(to) {
			case "in":
				scope = "IN_SCOPE"
			case "out":
				scope = "OUT_OF_SCOPE"
			default:
				return usagef("--to must be in or out")
			}
			path, err := a.itemPath("workflows", args[0])
			if err != nil {
				return err
			}
			o, err := w.options()
			if err != nil {
				return err
			}
			body := api.SetScopeRequest{Scope: scope,
				Etag: o.Etag, Note: o.Note, Reason: o.Reason, RequestId: o.RequestId, ValidateOnly: o.ValidateOnly}
			if len(evidence) > 0 {
				body.EvidenceAttachments = &evidence
			}
			if into != "" {
				n, err := a.fullName("workflows", into)
				if err != nil {
					return err
				}
				body.MergedInto = &n
			}
			out, err := a.call("POST", path+":setScope", nil, body)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "in or out")
	cmd.Flags().StringVar(&into, "into", "", "target workflow when --reason merged")
	bindEvidence(cmd, &evidence)
	w.bind(cmd)
	return cmd
}
