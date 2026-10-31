package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

func (a *App) taskCmd() *cobra.Command {
	return group("task", "Create, read, update and assign tasks",
		a.taskCreate(), a.taskGet(), a.taskList(), a.taskUpdate(), a.taskAssign())
}

func (a *App) taskCreate() *cobra.Command {
	var e entityFlags
	var w writeFlags
	var wfs []string
	cmd := &cobra.Command{
		Use: "create --name NAME [--workflow WF]...", Short: "Create a task (the first --workflow becomes primary)",
		Args: exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			if e.name == "" {
				return usagef("--name is required")
			}
			if err := w.noBody("create"); err != nil {
				return err
			}
			labels, ann, asg, err := e.maps()
			if err != nil {
				return err
			}
			body := api.Task{DisplayName: e.name}
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
			if len(wfs) > 0 {
				var ms []api.TaskMembership
				for i, wf := range wfs {
					n, err := a.fullName("workflows", wf)
					if err != nil {
						return err
					}
					ms = append(ms, api.TaskMembership{Workflow: n, Primary: ptr(i == 0)})
				}
				body.Workflows = &ms
			}
			path, err := a.collectionPath("tasks")
			if err != nil {
				return err
			}
			q, err := w.query(false)
			if err != nil {
				return err
			}
			if e.id != "" {
				q.Set("task_id", strings.ToLower(e.id))
			}
			out, err := a.call("POST", path, q, body)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	e.bind(cmd, true)
	cmd.Flags().StringArrayVar(&wfs, "workflow", nil, "workflow to attach to (repeatable)")
	w.bind(cmd)
	return cmd
}

func (a *App) taskGet() *cobra.Command {
	return &cobra.Command{
		Use: "get TK", Short: "Get a task", Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.itemPath("tasks", args[0])
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

func (a *App) taskList() *cobra.Command {
	var l listFlags
	var wf string
	cmd := &cobra.Command{
		Use: "list [--workflow WF] [--filter EXPR] [--page-count N]", Short: "List tasks",
		Args: exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := a.collectionPath("tasks")
			if err != nil {
				return err
			}
			extra := ""
			if wf != "" {
				n, err := a.fullName("workflows", wf)
				if err != nil {
					return err
				}
				extra = fmt.Sprintf("workflows.workflow = %q", n)
			}
			return a.listCall(path, &l, extra)
		},
	}
	l.bind(cmd, true)
	cmd.Flags().StringVar(&wf, "workflow", "", "only tasks attached to this workflow")
	return cmd
}

func (a *App) taskUpdate() *cobra.Command {
	var e entityFlags
	var w writeFlags
	cmd := &cobra.Command{
		Use: "update TK [--name N] [--label k=v] [--annotation k=v] [--assignee role=account]", Short: "Update a task (each given field is replaced as a whole)",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := a.itemPath("tasks", args[0])
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
			out, err := a.call("PATCH", path, map[string][]string{"update_mask": {mask}}, updateBody{MutationOptions: o, Task: fields})
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

func (a *App) taskAssign() *cobra.Command {
	var w writeFlags
	var wf, membership string
	cmd := &cobra.Command{
		Use: "assign TK --workflow WF [--membership primary|shared|none]", Short: "Attach a task to a workflow, change its membership, or detach (none)",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if wf == "" {
				return usagef("--workflow is required")
			}
			m := api.AssignTaskRequestMembership(strings.ToUpper(membership))
			if !m.Valid() || m == "MEMBERSHIP_UNSPECIFIED" {
				return usagef("--membership must be primary, shared or none")
			}
			path, err := a.itemPath("tasks", args[0])
			if err != nil {
				return err
			}
			name, err := a.fullName("workflows", wf)
			if err != nil {
				return err
			}
			o, err := w.options()
			if err != nil {
				return err
			}
			body := api.AssignTaskRequest{Workflow: name, Membership: m,
				Etag: o.Etag, Note: o.Note, Reason: o.Reason, RequestId: o.RequestId, ValidateOnly: o.ValidateOnly}
			out, err := a.call("POST", path+":assign", nil, body)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	cmd.Flags().StringVar(&wf, "workflow", "", "workflow to attach to / detach from")
	cmd.Flags().StringVar(&membership, "membership", "primary", "primary, shared or none (detach)")
	w.bind(cmd)
	return cmd
}
