package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
	"github.com/dlir2404/agent-shiftboard/internal/render"
)

func (a *app) configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Shared non-secret config values (not policy)"}
	get := &cobra.Command{
		Use: "get [name]", Short: "Show config values", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, _ coord.Actor) error {
				r, err := c.GetResources(ctx)
				if err != nil {
					return err
				}
				var out []coord.ConfigValue
				for _, v := range r.Config {
					if len(args) == 0 || v.Name == args[0] {
						out = append(out, v)
					}
				}
				if len(args) == 1 && len(out) == 0 {
					return fmt.Errorf("config %s not set", args[0])
				}
				return a.emit(out, func(w io.Writer) {
					for _, v := range out {
						fmt.Fprintf(w, "%s (%s) = %s\n", v.Name, v.Scope, v.Value)
					}
				})
			})
		},
	}
	var scope, desc string
	set := &cobra.Command{
		Use: "set <name> <value>", Short: "Set a non-secret value", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.SetConfig(ctx, actor, coord.ConfigValue{Name: args[0], Scope: scope, Value: args[1], Description: desc}); err != nil {
					return err
				}
				return a.done("Set "+args[0]+".", nil)
			})
		},
	}
	set.Flags().StringVar(&scope, "scope", "project", "project | user (user scope: user only)")
	set.Flags().StringVar(&desc, "description", "", "what the value is for")
	cmd.AddCommand(get, set)
	return cmd
}

func (a *app) secretCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "Secret references (locations and handling rules, never values)"}
	ref := &cobra.Command{Use: "ref", Short: "Manage secret references"}
	var s coord.SecretRef
	var keys string
	add := &cobra.Command{
		Use: "add <name>", Short: "Record where a secret lives and how to handle it", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s.Name = args[0]
			if keys != "" {
				s.Keys = strings.Split(keys, ",")
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.AddSecretRef(ctx, actor, s); err != nil {
					return err
				}
				return a.done("Recorded secret reference "+s.Name+".", nil)
			})
		},
	}
	add.Flags().StringVar(&s.Scope, "scope", "project", "project | user")
	add.Flags().StringVar(&s.Location, "location", "", "where it lives, e.g. ~/.config/app/dev-db.env (required)")
	add.Flags().StringVar(&keys, "keys", "", "comma-separated key names it provides")
	add.Flags().StringVar(&s.Purpose, "purpose", "", "what it is for")
	add.Flags().StringVar(&s.Handling, "handling", "", "handling rule, e.g. \"read-only use; never print\" (required)")
	list := &cobra.Command{
		Use: "list", Short: "List secret references", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, _ coord.Actor) error {
				r, err := c.GetResources(ctx)
				if err != nil {
					return err
				}
				return a.emit(r.Secrets, func(w io.Writer) {
					var rows [][]string
					for _, s := range r.Secrets {
						rows = append(rows, []string{s.Name, s.Scope, s.Location, strings.Join(s.Keys, ","), s.Handling})
					}
					table(w, "NAME\tSCOPE\tLOCATION\tKEYS\tHANDLING", rows)
				})
			})
		},
	}
	ref.AddCommand(add, list)
	cmd.AddCommand(ref)
	return cmd
}

func (a *app) toolCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tool", Short: "Shared tools any provider session can run"}
	list := &cobra.Command{
		Use: "list", Short: "List tools", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, _ coord.Actor) error {
				r, err := c.GetResources(ctx)
				if err != nil {
					return err
				}
				return a.emit(r.Tools, func(w io.Writer) {
					var rows [][]string
					for _, t := range r.Tools {
						rows = append(rows, []string{t.Name, t.Scope, t.Status, t.Effect, trunc(t.Description, 60)})
					}
					table(w, "NAME\tSCOPE\tSTATUS\tEFFECT\tDESCRIPTION", rows)
				})
			})
		},
	}
	show := &cobra.Command{
		Use: "show <name>", Short: "Show a tool's manifest", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, _ coord.Actor) error {
				r, err := c.GetResources(ctx)
				if err != nil {
					return err
				}
				for _, t := range r.Tools {
					if t.Name == args[0] {
						return a.emit(t, func(w io.Writer) {
							fmt.Fprintf(w, "%s (%s, %s, %s)\n%s\ncommand: %s\n", t.Name, t.Scope, t.Status, t.Effect, t.Description, t.Command)
							if t.Usage != "" {
								fmt.Fprintf(w, "usage: %s\n", t.Usage)
							}
							if t.Targets != "" {
								fmt.Fprintf(w, "targets: %s\n", t.Targets)
							}
							fmt.Fprintf(w, "requires: config=%v secrets=%v executables=%v\n", t.Requires.Config, t.Requires.Secrets, t.Requires.Executables)
							if len(t.Locks) > 0 {
								fmt.Fprintf(w, "locks: %s\n", strings.Join(t.Locks, ", "))
							}
							if t.Effect == "external_write" {
								fmt.Fprintln(w, "Running it is a side effect: effect plan/start before, effect finish after.")
							}
						})
					}
				}
				return fmt.Errorf("tool %s not found", args[0])
			})
		},
	}
	var t coord.Tool
	var reqConfig, reqSecrets, reqExec, locks string
	register := &cobra.Command{
		Use: "register <name>", Short: "Register a reusable helper", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t.Name = args[0]
			t.Requires = coord.ToolRequires{Config: splitList(reqConfig), Secrets: splitList(reqSecrets), Executables: splitList(reqExec)}
			t.Locks = splitList(locks)
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.RegisterTool(ctx, actor, t); err != nil {
					return err
				}
				return a.done("Registered tool "+t.Name+".", nil)
			})
		},
	}
	f := register.Flags()
	f.StringVar(&t.Scope, "scope", "project", "project | user")
	f.StringVar(&t.Description, "description", "", "one-line description (required)")
	f.StringVar(&t.Command, "command", "", "command or path (not in a provider-private folder)")
	f.StringVar(&t.Usage, "usage", "", "how to call it")
	f.StringVar(&t.Effect, "effect", "read_only", "read_only | local_write | external_write")
	f.StringVar(&t.Targets, "targets", "", "what it touches")
	f.StringVar(&reqConfig, "requires-config", "", "comma-separated config names")
	f.StringVar(&reqSecrets, "requires-secrets", "", "comma-separated secret names")
	f.StringVar(&reqExec, "requires-executables", "", "comma-separated executables")
	f.StringVar(&locks, "locks", "", "comma-separated lock names it takes")
	f.StringVar(&t.VerifiedAt, "verified-at", "", "when it last ran successfully")
	var depScope, supersededBy string
	deprecate := &cobra.Command{
		Use: "deprecate <name>", Short: "Deprecate a tool (or --superseded-by another)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				if err := c.DeprecateTool(ctx, actor, args[0], depScope, supersededBy); err != nil {
					return err
				}
				return a.done("Tool "+args[0]+" updated.", nil)
			})
		},
	}
	deprecate.Flags().StringVar(&depScope, "scope", "project", "project | user")
	deprecate.Flags().StringVar(&supersededBy, "superseded-by", "", "replacement tool")
	cmd.AddCommand(list, show, register, deprecate)
	return cmd
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (a *app) getCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "get", Short: "Export registries"}
	var format, scope string
	res := &cobra.Command{
		Use: "resources", Short: "Show shared resources (-o toml exports a manifest)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, _ coord.Actor) error {
				if format == "toml" {
					b, err := c.ExportManifest(ctx, scope)
					if err != nil {
						return err
					}
					_, err = a.out.Write(b)
					return err
				}
				r, err := c.GetResources(ctx)
				if err != nil {
					return err
				}
				return a.emit(r, func(w io.Writer) { fmt.Fprint(w, render.ResourceIndex(r)) })
			})
		},
	}
	res.Flags().StringVarP(&format, "output", "o", "", "toml to export a manifest")
	res.Flags().StringVar(&scope, "scope", "project", "scope to export with -o toml")
	cmd.AddCommand(res)
	return cmd
}

func (a *app) applyCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use: "apply", Short: "Apply a resource manifest (user)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var data []byte
			var err error
			if file == "-" {
				data, err = io.ReadAll(os.Stdin)
			} else {
				data, err = os.ReadFile(file)
			}
			if err != nil {
				return err
			}
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
				n, err := c.ApplyManifest(ctx, actor, data)
				if err != nil {
					return err
				}
				return a.done(fmt.Sprintf("Applied %d resources.", n), map[string]int{"applied": n})
			})
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "manifest file, or - for stdin")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func (a *app) lockCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "lock", Short: "Named advisory locks on shared external resources"}
	list := &cobra.Command{
		Use: "list", Short: "List held and stale locks", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, _ coord.Actor) error {
				ls, err := c.ListLocks(ctx)
				if err != nil {
					return err
				}
				return a.emit(ls, func(w io.Writer) {
					var rows [][]string
					for _, l := range ls {
						rows = append(rows, []string{l.Name, l.Status, l.Holder, l.AcquiredAt})
					}
					table(w, "LOCK\tSTATUS\tHOLDER\tSINCE", rows)
				})
			})
		},
	}
	op := func(use, short string, fn func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, name string) error) *cobra.Command {
		return &cobra.Command{Use: use + " <name>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return a.with(cmd, func(ctx context.Context, c *coord.Coordinator, actor coord.Actor) error {
					if err := fn(ctx, c, actor, args[0]); err != nil {
						return err
					}
					return a.done("Lock "+args[0]+": "+use+" done.", nil)
				})
			}}
	}
	cmd.AddCommand(list,
		op("acquire", "Take a lock", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, n string) error {
			return c.AcquireLock(ctx, actor, n)
		}),
		op("release", "Release a lock you hold", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, n string) error {
			return c.ReleaseLock(ctx, actor, n)
		}),
		op("break", "Release any holder's lock (user)", func(ctx context.Context, c *coord.Coordinator, actor coord.Actor, n string) error {
			return c.BreakLock(ctx, actor, n)
		}),
	)
	return cmd
}
