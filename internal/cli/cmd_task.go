package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

func (a *App) taskCmd() *cobra.Command {
	return group("task", "Create, read, update, move, delete and restore tasks",
		a.taskCreate(), a.taskGet(), a.taskList(), a.taskUpdate(), a.taskAssign(),
		a.taskDelete(), a.taskRestore())
}

func (a *App) taskCreate() *cobra.Command {
	var e entityFlags
	var w writeFlags
	var wf string
	cmd := &cobra.Command{
		Use: "create --name NAME --workflow WF", Short: "Create a task in a workflow",
		Args: exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			if e.name == "" {
				return usagef("--name is required")
			}
			if wf == "" {
				return usagef("--workflow is required (a task belongs to exactly one workflow)")
			}
			if err := w.noBody("create"); err != nil {
				return err
			}
			labels, ann, asg, err := e.maps()
			if err != nil {
				return err
			}
			wfName, err := a.fullName("workflows", wf)
			if err != nil {
				return err
			}
			body := api.Task{DisplayName: e.name, Workflow: wfName}
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
	cmd.Flags().StringVar(&wf, "workflow", "", "workflow the task belongs to")
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
				extra = fmt.Sprintf("workflow = %q", n)
			}
			return a.listCall(path, &l, extra)
		},
	}
	l.bind(cmd, true)
	cmd.Flags().StringVar(&wf, "workflow", "", "only tasks of this workflow")
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
	var wf string
	cmd := &cobra.Command{
		Use: "assign TK --workflow WF", Short: "Move a task to another workflow",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if wf == "" {
				return usagef("--workflow is required")
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
			body := api.AssignTaskRequest{Workflow: name,
				Etag: o.Etag, Note: o.Note, Reason: o.Reason, RequestId: o.RequestId, ValidateOnly: o.ValidateOnly}
			out, err := a.call("POST", path+":assign", nil, body)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	cmd.Flags().StringVar(&wf, "workflow", "", "workflow to move the task to")
	w.bind(cmd)
	return cmd
}
