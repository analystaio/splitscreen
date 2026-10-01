package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/analystaio/splitscreen/config"
)

// Routing is edited through this command rather than by hand so the common case
// is validated before it lands. It is still a file, still reviewable, and still
// the single source of truth — there is deliberately no chat command that
// mutates routing, because which humans can drive which machines is not a
// decision that should be typed by whoever is in the channel at the time.

func routeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Inspect and edit the routing table",
	}
	cmd.AddCommand(routeListCmd(), routeAddCmd(), routeRemoveCmd())
	return cmd
}

func routeListCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show which channels route to which runners",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if len(cfg.Routes) == 0 {
				fmt.Println("No routes. Every channel is ignored.")
				return nil
			}
			byRunner := map[string][]string{}
			for _, r := range cfg.Routes {
				target := r.Channel
				if r.DM {
					target = "<direct messages>"
				}
				byRunner[r.Runner] = append(byRunner[r.Runner], target)
			}
			names := make([]string, 0, len(byRunner))
			for n := range byRunner {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Printf("%s\n", n)
				for _, t := range byRunner[n] {
					fmt.Printf("    %s\n", t)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	return cmd
}

func routeAddCmd() *cobra.Command {
	var cfgPath string
	var dm bool

	cmd := &cobra.Command{
		Use:   "add <channel-id> <runner>",
		Short: "Route a channel to a runner",
		Long: `Adds a route and validates the result before writing.

The file is edited in place through the YAML node tree, so comments and
formatting elsewhere survive untouched. The gateway is not signalled; reload it
when you are ready.

Remember that Slack only delivers messages for channels the bot has joined — a
route without an invite looks exactly like no route at all.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if dm {
				return cobra.ExactArgs(1)(cmd, args)
			}
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			channel, runner := "", args[0]
			if !dm {
				channel, runner = args[0], args[1]
			}

			unlock, err := lockConfig(cfgPath)
			if err != nil {
				return err
			}
			defer unlock()

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("the existing config is not valid; fix it before adding a route:\n%w", err)
			}
			if _, ok := cfg.Runners[runner]; !ok {
				return fmt.Errorf("no runner named %q is configured", runner)
			}
			for _, r := range cfg.Routes {
				if !dm && r.Channel == channel {
					return fmt.Errorf("channel %s already routes to %q — a channel maps to exactly one runner", channel, r.Runner)
				}
				if dm && r.DM {
					return fmt.Errorf("direct messages already route to %q; there is only one DM surface", r.Runner)
				}
			}

			updated, err := editRoutes(cfgPath, func(seq *yaml.Node) error {
				seq.Content = append(seq.Content, routeNode(channel, runner, dm))
				return nil
			})
			if err != nil {
				return err
			}
			if _, err := config.Parse(updated); err != nil {
				return fmt.Errorf("the edit would produce an invalid config; nothing was written:\n%w", err)
			}
			if err := writeConfig(cfgPath, updated); err != nil {
				return err
			}

			target := channel
			if dm {
				target = "direct messages"
			}
			fmt.Printf("Routed %s -> %s in %s\n\n", target, runner, cfgPath)
			fmt.Printf("Apply it:      systemctl reload splitscreen-gateway\n")
			if !dm {
				fmt.Printf("Then in Slack: /invite the bot into the channel, or nothing will arrive.\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	cmd.Flags().BoolVar(&dm, "dm", false, "route direct messages instead of a channel")
	return cmd
}

func routeRemoveCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "remove <channel-id>",
		Short: "Stop routing a channel",
		Long: `Removes a route. Existing threads keep their runner — their sessions live
on that runner's disk — so this only affects new conversations.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channel := args[0]

			unlock, err := lockConfig(cfgPath)
			if err != nil {
				return err
			}
			defer unlock()

			var found bool
			updated, err := editRoutes(cfgPath, func(seq *yaml.Node) error {
				kept := seq.Content[:0]
				for _, item := range seq.Content {
					if nodeMapValue(item, "channel") == channel {
						found = true
						continue
					}
					kept = append(kept, item)
				}
				seq.Content = kept
				return nil
			})
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("no route for channel %s", channel)
			}
			if _, err := config.Parse(updated); err != nil {
				return fmt.Errorf("the edit would produce an invalid config; nothing was written:\n%w", err)
			}
			if err := writeConfig(cfgPath, updated); err != nil {
				return err
			}
			fmt.Printf("Removed the route for %s from %s\n", channel, cfgPath)
			fmt.Printf("Apply it: systemctl reload splitscreen-gateway\n")
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	return cmd
}

const defaultConfigPath = "splitscreen.yaml"

// configLockTimeout bounds how long an edit waits for a concurrent one. Edits
// take milliseconds; anything holding the lock this long is stuck.
const configLockTimeout = 30 * time.Second

// policyCmd is what the removed "Always" button would have done, except it
// leaves a diff. Permission posture is a security boundary; widening it should
// be a reviewable edit, not a click by whoever happened to be in the channel.
func policyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Inspect and edit per-runner permission policy",
	}
	cmd.AddCommand(policyShowCmd(), policyRuleCmd("allow"), policyRuleCmd("deny"))
	return cmd
}

func policyShowCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show how each runner handles permission requests",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(cfg.Runners))
			for n := range cfg.Runners {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				r := cfg.Runners[n]
				mode := "prompting"
				if r.Policy.AutoApprove {
					mode = "unattended"
				}
				fmt.Printf("%s — %s\n", n, mode)
				for _, d := range r.Policy.Deny {
					fmt.Printf("    deny   %s\n", d)
				}
				for _, a := range r.Policy.Allow {
					fmt.Printf("    allow  %s\n", a)
				}
				if r.Policy.AutoApprove && len(r.Policy.Deny) == 0 {
					fmt.Printf("    WARNING: unattended with no deny rules — every tool call proceeds unchecked\n")
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	return cmd
}

func policyRuleCmd(kind string) *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   kind + " <runner> <rule>",
		Short: "Add " + kind + " rule to a runner, e.g. 'Bash(git status*)'",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, rule := args[0], args[1]
			unlock, err := lockConfig(cfgPath)
			if err != nil {
				return err
			}
			defer unlock()

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("the existing config is not valid; fix it first:\n%w", err)
			}
			rc, ok := cfg.Runners[runner]
			if !ok {
				return fmt.Errorf("no runner named %q is configured", runner)
			}
			existing := rc.Policy.Allow
			if kind == "deny" {
				existing = rc.Policy.Deny
			}
			for _, e := range existing {
				if e == rule {
					return fmt.Errorf("runner %q already has that %s rule", runner, kind)
				}
			}

			updated, err := editRunnerPolicy(cfgPath, runner, kind, rule)
			if err != nil {
				return err
			}
			if _, err := config.Parse(updated); err != nil {
				return fmt.Errorf("the edit would produce an invalid config; nothing was written:\n%w", err)
			}
			if err := writeConfig(cfgPath, updated); err != nil {
				return err
			}
			fmt.Printf("Added %s rule %q to %s in %s\n\n", kind, rule, runner, cfgPath)
			fmt.Printf("Apply it: systemctl reload splitscreen-gateway\n")
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	return cmd
}

// editRunnerPolicy appends a rule through the node tree, so the file's comments
// survive the edit.
func editRunnerPolicy(path, runner, kind, rule string) ([]byte, error) {
	return editDoc(path, func(root *yaml.Node) error {
		runners := mappingValue(root, "runners")
		if runners == nil {
			return fmt.Errorf("config: no runners section")
		}
		rc := mappingValue(runners, runner)
		if rc == nil {
			return fmt.Errorf("config: runner %q not found in the file", runner)
		}
		policy := mappingValue(rc, "policy")
		if policy == nil {
			rc.Content = append(rc.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "policy"},
				&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
			policy = rc.Content[len(rc.Content)-1]
		}
		list := mappingValue(policy, kind)
		if list == nil {
			policy.Content = append(policy.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kind},
				&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
			list = policy.Content[len(policy.Content)-1]
		}
		list.Content = append(list.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: rule, Style: yaml.DoubleQuotedStyle})
		return nil
	})
}

// editRoutes applies fn to the routes sequence and returns the re-serialized
// document.
func editRoutes(path string, fn func(seq *yaml.Node) error) ([]byte, error) {
	return editDoc(path, func(root *yaml.Node) error {
		seq, err := routesSeq(root)
		if err != nil {
			return err
		}
		return fn(seq)
	})
}

// routesSeq returns the routes sequence, creating an empty one if absent.
func routesSeq(root *yaml.Node) (*yaml.Node, error) {
	seq := mappingValue(root, "routes")
	if seq == nil {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "routes"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"},
		)
		seq = root.Content[len(root.Content)-1]
	}
	if seq.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("config: routes is not a list")
	}
	return seq, nil
}

// editDoc applies fn to the document's root mapping and returns the
// re-serialized document. Editing the node tree rather than round-tripping
// through structs is what keeps the file's comments — which carry most of its
// explanation — intact.
func editDoc(path string, fn func(root *yaml.Node) error) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config: %s is not a YAML mapping", path)
	}
	if err := fn(doc.Content[0]); err != nil {
		return nil, err
	}

	var out strings.Builder
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func nodeMapValue(m *yaml.Node, key string) string {
	if m.Kind != yaml.MappingNode {
		return ""
	}
	if v := mappingValue(m, key); v != nil {
		return v.Value
	}
	return ""
}

func routeNode(channel, runner string, dm bool) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
	add := func(k string, v *yaml.Node) {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, v)
	}
	if dm {
		add("dm", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	} else {
		add("channel", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: channel})
	}
	add("runner", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: runner})
	return n
}

// writeConfig replaces the file atomically, so an interrupted write cannot leave
// the gateway with a truncated config to reload.
func writeConfig(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".splitscreen-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if info, err := os.Stat(path); err == nil {
		if err := tmp.Chmod(info.Mode().Perm()); err != nil {
			tmp.Close()
			return err
		}
		if err := preserveOwner(tmp, info); err != nil {
			tmp.Close()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
