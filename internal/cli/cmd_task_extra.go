package cli

import "github.com/spf13/cobra"

func (a *App) taskDelete() *cobra.Command {
	var w writeFlags
	cmd := &cobra.Command{
		Use: "delete TK", Short: "Soft-delete a task", Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.itemPath("tasks", args[0])
			if err != nil {
				return err
			}
			if w.reason != "" || w.note != "" {
				return usagef("--reason and --note are not supported by task delete (the API has no field for them)")
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

func (a *App) taskRestore() *cobra.Command {
	var w writeFlags
	cmd := &cobra.Command{
		Use: "restore TK", Short: "Restore a soft-deleted task", Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.itemPath("tasks", args[0])
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
