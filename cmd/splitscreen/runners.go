package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/protocol"
)

// Runner definitions are edited through these commands for the same reason
// routes are: the common case is validated before it lands, and the file stays
// the single reviewable source of truth. They exist so a control plane that
// creates and destroys short-lived runners — one per task machine — can
// register them without hand-editing YAML, and without two registrations
// racing each other into a lost edit.

// runnerManageCmds are attached under `splitscreen runner`, beside the daemon
// itself: the daemon takes only flags, so the subcommand names never collide
// with anything it accepts.
func runnerManageCmds() []*cobra.Command {
	return []*cobra.Command{runnerListCmd(), runnerAddCmd(), runnerRemoveCmd()}
}

// identityKeys are never copied from a template. Each names something that must
// be unique to one runner: sharing a token secret would let either runner
// authenticate as the other. The wake target is the same kind of thing and is
// handled separately — a template's wake block (its region) is copied, but its
// ec2_instance never is; the copy's comes from --set or from its host.
var identityKeys = []string{"token_secret"}

func runnerListCmd() *cobra.Command {
	var cfgPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show the configured runners",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			type entry struct {
				Name   string   `json:"name"`
				Host   string   `json:"host,omitempty"`
				Cwd    string   `json:"cwd"`
				Bundle string   `json:"bundle,omitempty"`
				Wake   string   `json:"wake,omitempty"`
				Routes []string `json:"routes"`
			}
			names := make([]string, 0, len(cfg.Runners))
			for n := range cfg.Runners {
				names = append(names, n)
			}
			sort.Strings(names)
			out := make([]entry, 0, len(names))
			for _, n := range names {
				r := cfg.Runners[n]
				e := entry{Name: n, Host: r.Host, Cwd: r.Cwd, Bundle: r.Bundle, Routes: []string{}}
				if r.Wakeable() {
					e.Wake = r.Wake.EC2Instance
				}
				for _, rt := range cfg.Routes {
					if rt.Runner != n {
						continue
					}
					if rt.DM {
						e.Routes = append(e.Routes, "<dm>")
					} else {
						e.Routes = append(e.Routes, rt.Channel)
					}
				}
				out = append(out, e)
			}

			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			for _, e := range out {
				fmt.Printf("%s\n", e.Name)
				fmt.Printf("    cwd %s", e.Cwd)
				if e.Host != "" {
					fmt.Printf(", host %s", e.Host)
				}
				if e.Wake != "" {
					fmt.Printf(", wakes %s", e.Wake)
				}
				fmt.Println()
				if len(e.Routes) == 0 {
					fmt.Printf("    (no routes)\n")
				}
				for _, rt := range e.Routes {
					fmt.Printf("    <- %s\n", rt)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func runnerAddCmd() *cobra.Command {
	var cfgPath, template string
	var sets []string

	cmd := &cobra.Command{
		Use:   "add <name> --template <runner> [--set key=value ...]",
		Short: "Add a runner by copying an existing one",
		Long: `Copies an existing runner's definition under a new name, applies overrides,
and validates the whole result before writing.

The template is usually a runner kept for this purpose and routed nowhere, so
policy, bundle, and harness settings are written once and every copy inherits
them. Its token_secret is never copied. If it has a wake block, the block (its
region) is copied but its ec2_instance never is: the copy wakes the instance
given by --set wake.ec2_instance, or else its host when that is an instance id,
so --set host=i-… alone yields a wakeable runner.

--set takes a dotted path to a scalar field and may repeat:

    --set host=i-0abc123 --set display.name="Box foo" --set wake.ec2_instance=i-0abc123

Missing intermediate sections are created. Values are typed the way YAML would
read them (4 is a number, true a boolean), except where the template already
holds a string. Lists cannot be set this way; edit the template instead.

Fails if the name is taken. The gateway is not signalled; reload it when ready.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !protocol.ValidSlug(name) {
				return fmt.Errorf("%q is not a valid runner name (lowercase letters, digits, dashes)", name)
			}
			if template == "" {
				return errors.New("--template is required")
			}
			overrides, err := parseSets(sets)
			if err != nil {
				return err
			}
			setKeys := map[string]bool{}
			for _, o := range overrides {
				setKeys[strings.Join(o.path, ".")] = true
			}

			unlock, err := lockConfig(cfgPath)
			if err != nil {
				return err
			}
			defer unlock()

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("the existing config is not valid; fix it before adding a runner:\n%w", err)
			}
			if _, exists := cfg.Runners[name]; exists {
				return fmt.Errorf("a runner named %q already exists", name)
			}
			if _, ok := cfg.Runners[template]; !ok {
				return fmt.Errorf("no template runner named %q is configured", template)
			}

			updated, err := editDoc(cfgPath, func(root *yaml.Node) error {
				runners := mappingValue(root, "runners")
				if runners == nil || runners.Kind != yaml.MappingNode {
					return errors.New("config: no runners section")
				}
				src := mappingValue(runners, template)
				if src == nil || src.Kind != yaml.MappingNode {
					return fmt.Errorf("config: runner %q is not a mapping in the file", template)
				}
				block := cloneNode(src)
				stripComments(block)
				block.Style = 0 // a flow-style template would render the copy on one line
				for _, k := range identityKeys {
					deleteKey(block, k)
				}
				if w := mappingValue(block, "wake"); w != nil && w.Kind == yaml.MappingNode {
					deleteKey(w, "ec2_instance")
				}
				for _, o := range overrides {
					if err := setPath(block, o.path, o.value); err != nil {
						return fmt.Errorf("--set %s: %w", strings.Join(o.path, "."), err)
					}
				}
				_, hostSet := setKeys["host"]
				if err := fillWakeFromHost(block, hostSet); err != nil {
					return err
				}
				key := &yaml.Node{
					Kind: yaml.ScalarNode, Tag: "!!str", Value: name,
					HeadComment: fmt.Sprintf("Added by `splitscreen runner add` from %s, %s.",
						template, time.Now().UTC().Format(time.RFC3339)),
				}
				runners.Content = append(runners.Content, key, block)
				return nil
			})
			if err != nil {
				return err
			}
			next, err := config.Parse(updated)
			if err != nil {
				return fmt.Errorf("the edit would produce an invalid config; nothing was written:\n%w", err)
			}

			// A secret file left behind by an earlier runner of the same name
			// wins over Parameter Store, so it would silently reject the token
			// the new runner is given. At this point no runner by this name is
			// configured, so any such file is stale by construction — but
			// deleting a credential file is not this command's call to make.
			rc := next.Runners[name]
			if dir := next.Gateway.SecretsDir; dir != "" {
				stale := filepath.Join(dir, rc.EffectiveTokenSecret(name))
				if _, err := os.Stat(stale); err == nil {
					return fmt.Errorf("%s already exists and would shadow any other secret backend; "+
						"it belongs to an earlier runner of this name — remove it (or run `splitscreen runner remove`) first", stale)
				}
			}

			if err := writeConfig(cfgPath, updated); err != nil {
				return err
			}
			fmt.Printf("Added runner %s (from %s) to %s\n", name, template, cfgPath)
			fmt.Printf("Enrollment secret: %s\n", rc.EffectiveTokenSecret(name))
			if rc.Wakeable() {
				fmt.Printf("Wakes:             %s\n", rc.Wake.EC2Instance)
			}
			fmt.Printf("\nNext: route a channel to it, then systemctl reload splitscreen-gateway\n")
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	cmd.Flags().StringVar(&template, "template", "", "existing runner to copy")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "override a scalar field, e.g. display.name=Foo (repeatable)")
	return cmd
}

func runnerRemoveCmd() *cobra.Command {
	var cfgPath string
	var missingOK, keepSecret bool

	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a runner and every route to it",
		Long: `Removes a runner's definition and every route pointing at it, in one
validated edit — a route to a runner that does not exist is invalid, so the two
cannot be removed separately in either order.

On reload the gateway closes the runner's connection and refuses its token from
then on: an unconfigured runner cannot authenticate, whatever it presents.

The runner's enrollment secret file in gateway.secrets_dir, if any, is deleted
too, so a later runner of the same name cannot be shadowed by it. Secrets held in
another backend (Parameter Store) are the caller's to delete.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			unlock, err := lockConfig(cfgPath)
			if err != nil {
				return err
			}
			defer unlock()

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("the existing config is not valid; fix it before removing a runner:\n%w", err)
			}
			rc, ok := cfg.Runners[name]
			if !ok {
				if missingOK {
					fmt.Printf("No runner named %s; nothing to do.\n", name)
					return nil
				}
				return fmt.Errorf("no runner named %q is configured", name)
			}
			secretName := rc.EffectiveTokenSecret(name)

			var dropped []string
			updated, err := editDoc(cfgPath, func(root *yaml.Node) error {
				runners := mappingValue(root, "runners")
				if runners == nil || !deleteKey(runners, name) {
					return fmt.Errorf("config: runner %q not found in the file", name)
				}
				seq, err := routesSeq(root)
				if err != nil {
					return err
				}
				kept := seq.Content[:0]
				for _, item := range seq.Content {
					if nodeMapValue(item, "runner") == name {
						target := nodeMapValue(item, "channel")
						if target == "" {
							target = "<dm>"
						}
						dropped = append(dropped, target)
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
			if _, err := config.Parse(updated); err != nil {
				return fmt.Errorf("the edit would produce an invalid config; nothing was written:\n%w", err)
			}
			if err := writeConfig(cfgPath, updated); err != nil {
				return err
			}

			fmt.Printf("Removed runner %s from %s\n", name, cfgPath)
			for _, d := range dropped {
				fmt.Printf("    and its route from %s\n", d)
			}
			if dir := cfg.Gateway.SecretsDir; dir != "" && !keepSecret {
				for _, f := range []string{secretName, secretName + ".expires"} {
					p := filepath.Join(dir, f)
					if err := os.Remove(p); err == nil {
						fmt.Printf("Deleted %s\n", p)
					} else if !os.IsNotExist(err) {
						// The config edit already landed and is what revokes
						// access; a leftover file is a hygiene problem, not a
						// hole, so report it rather than fail.
						fmt.Fprintf(os.Stderr, "warning: could not delete %s: %v\n", p, err)
					}
				}
			}
			fmt.Printf("\nApply it: systemctl reload splitscreen-gateway\n")
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	cmd.Flags().BoolVar(&missingOK, "missing-ok", false, "succeed when the runner is already gone (for idempotent callers)")
	cmd.Flags().BoolVar(&keepSecret, "keep-secret", false, "leave the runner's secret file in gateway.secrets_dir")
	return cmd
}

// fillWakeFromHost completes a copied wake block. A template declares that its
// copies are wakeable (and in which region) without naming a machine; each copy
// names its own, either explicitly with --set wake.ec2_instance or — the common
// case — through its host, when that is an instance id. A wake block left with
// no instance is an error here rather than a silently unwakeable runner.
//
// The host must have been given for this copy: a host inherited from the
// template would make the copy wake the template's machine.
func fillWakeFromHost(block *yaml.Node, hostSet bool) error {
	w := mappingValue(block, "wake")
	if w == nil || w.Kind != yaml.MappingNode {
		return nil
	}
	if id := mappingValue(w, "ec2_instance"); id != nil && id.Value != "" {
		return nil
	}
	host := nodeMapValue(block, "host")
	if !hostSet {
		return errors.New("the template declares wake, so each copy needs its own machine: " +
			"pass --set host=i-… (or --set wake.ec2_instance=i-…)")
	}
	if !config.IsEC2InstanceID(host) {
		return fmt.Errorf("the template declares wake, but host %q is not an instance id; "+
			"set --set host=i-… or --set wake.ec2_instance=i-…", host)
	}
	return setPath(w, []string{"ec2_instance"}, host)
}

// ---------------------------------------------------------------------------
// --set
// ---------------------------------------------------------------------------

type override struct {
	path  []string
	value string
}

var setKey = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

func parseSets(sets []string) ([]override, error) {
	var out []override
	seen := map[string]bool{}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q: expected key=value", s)
		}
		if !setKey.MatchString(k) {
			return nil, fmt.Errorf("--set %q: key must be a dotted path of lowercase field names", s)
		}
		if seen[k] {
			return nil, fmt.Errorf("--set %s given twice", k)
		}
		seen[k] = true
		out = append(out, override{path: strings.Split(k, "."), value: v})
	}
	return out, nil
}

// setPath writes a scalar at a dotted path, creating intermediate mappings.
func setPath(m *yaml.Node, path []string, value string) error {
	for i, key := range path {
		if m.Kind != yaml.MappingNode {
			return fmt.Errorf("%s is not a section", strings.Join(path[:i], "."))
		}
		cur := mappingValue(m, key)
		last := i == len(path)-1
		if last {
			tag := scalarTag(value)
			if cur != nil {
				if cur.Kind != yaml.ScalarNode {
					return errors.New("is not a scalar field; edit the template instead")
				}
				if cur.Tag == "!!str" {
					tag = "!!str"
				}
				*cur = yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
				return nil
			}
			m.Content = append(m.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
			return nil
		}
		if cur == nil {
			cur = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			m.Content = append(m.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, cur)
		}
		m = cur
	}
	return nil
}

// scalarTag is the tag YAML would resolve an unquoted value to, so that
// --set max_concurrent=4 lands as a number and --set model=claude-opus-5 as a
// string. Anything that does not parse as a plain scalar is a string.
func scalarTag(v string) string {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(v), &n); err != nil || len(n.Content) != 1 ||
		n.Content[0].Kind != yaml.ScalarNode || n.Content[0].Value != v {
		return "!!str"
	}
	switch t := n.Content[0].Tag; t {
	case "!!int", "!!bool", "!!float":
		return t
	default:
		return "!!str"
	}
}

func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = cloneNode(child)
	}
	return &c
}

// stripComments removes the template's commentary from the copy: it explains
// the template, and duplicated into every generated runner it would describe
// something that is not there.
func stripComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		stripComments(c)
	}
}

// deleteKey removes a key from a mapping, reporting whether it was present.
func deleteKey(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}
