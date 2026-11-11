package coord

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/dlir2404/agent-shiftboard/internal/secretscan"
	"github.com/dlir2404/agent-shiftboard/internal/store"
	"github.com/dlir2404/agent-shiftboard/internal/vocab"
)

// ConfigValue is a non-secret shared value.
type ConfigValue struct {
	Name        string `json:"name" toml:"-"`
	Scope       string `json:"scope" toml:"-"`
	Value       string `json:"value" toml:"value"`
	Description string `json:"description,omitempty" toml:"description,omitempty"`
}

// SecretRef says where a secret lives, never its value.
type SecretRef struct {
	Name     string   `json:"name" toml:"-"`
	Scope    string   `json:"scope" toml:"-"`
	Location string   `json:"location" toml:"location"`
	Keys     []string `json:"keys,omitempty" toml:"keys,omitempty"`
	Purpose  string   `json:"purpose,omitempty" toml:"purpose,omitempty"`
	Handling string   `json:"handling" toml:"handling"`
}

// ToolRequires lists what a tool needs to run.
type ToolRequires struct {
	Config      []string `json:"config,omitempty" toml:"config,omitempty"`
	Secrets     []string `json:"secrets,omitempty" toml:"secrets,omitempty"`
	Executables []string `json:"executables,omitempty" toml:"executables,omitempty"`
}

// Tool is a reusable helper any provider session can run.
type Tool struct {
	Name         string       `json:"name" toml:"-"`
	Scope        string       `json:"scope" toml:"-"`
	Description  string       `json:"description" toml:"description"`
	Command      string       `json:"command" toml:"command"`
	Usage        string       `json:"usage,omitempty" toml:"usage,omitempty"`
	Effect       string       `json:"effect_class" toml:"effect"`
	Targets      string       `json:"targets,omitempty" toml:"targets,omitempty"`
	Requires     ToolRequires `json:"requires" toml:"requires"`
	Locks        []string     `json:"locks,omitempty" toml:"locks,omitempty"`
	CreatedBy    string       `json:"created_by,omitempty" toml:"-"`
	VerifiedAt   string       `json:"verified_at,omitempty" toml:"verified_at,omitempty"`
	Status       string       `json:"status" toml:"status,omitempty"`
	SupersededBy string       `json:"superseded_by,omitempty" toml:"superseded_by,omitempty"`
}

// Resources is the full registry, as returned by `get resources`.
type Resources struct {
	Config  []ConfigValue `json:"config"`
	Secrets []SecretRef   `json:"secrets"`
	Tools   []Tool        `json:"tools"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)

func checkName(kind, n string) error {
	if !nameRe.MatchString(n) {
		return fmt.Errorf("%s name %q must be lowercase letters, digits, and . _ : - (max 64)", kind, n)
	}
	return nil
}

func (c *Coordinator) resourceDB(a Actor, scope string) (*sql.DB, error) {
	if err := vocab.ResourceScope.Check(scope); err != nil {
		return nil, err
	}
	if scope == "user" {
		if !a.IsUser() {
			return nil, errors.New("agents may register project-scope resources only; user-scope resources apply to every project and are set by the user")
		}
		return c.UserDB, nil
	}
	return c.DB, nil
}

// SetConfig sets a non-secret config value.
func (c *Coordinator) SetConfig(ctx context.Context, a Actor, v ConfigValue) error {
	if err := checkName("config", v.Name); err != nil {
		return err
	}
	if err := secretscan.Check("config value", v.Value+" "+v.Description); err != nil {
		return err
	}
	db, err := c.resourceDB(a, v.Scope)
	if err != nil {
		return err
	}
	return store.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO config_values(name,value,description,updated_by,updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET value=excluded.value,description=excluded.description,updated_by=excluded.updated_by,updated_at=excluded.updated_at`,
			v.Name, v.Value, v.Description, a.String(), c.now())
		return err
	})
}

// AddSecretRef records where a secret lives and how to handle it.
func (c *Coordinator) AddSecretRef(ctx context.Context, a Actor, s SecretRef) error {
	if err := checkName("secret", s.Name); err != nil {
		return err
	}
	if strings.TrimSpace(s.Location) == "" || strings.TrimSpace(s.Handling) == "" {
		return errors.New("location and handling rule are required (for example: \"read-only use; never print\")")
	}
	if err := secretscan.Check("secret reference", strings.Join(append([]string{s.Location, s.Purpose, s.Handling}, s.Keys...), " ")); err != nil {
		return err
	}
	db, err := c.resourceDB(a, s.Scope)
	if err != nil {
		return err
	}
	return store.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO secret_refs(name,location,keys,purpose,handling,updated_by,updated_at) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET location=excluded.location,keys=excluded.keys,purpose=excluded.purpose,handling=excluded.handling,updated_by=excluded.updated_by,updated_at=excluded.updated_at`,
			s.Name, s.Location, strings.Join(s.Keys, ","), s.Purpose, s.Handling, a.String(), c.now())
		return err
	})
}

// privateDirs are provider-private or temporary locations where a shared
// tool must not live (D-19).
func privateDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{"/tmp/", "/private/tmp/", "/var/folders/", filepath.Clean(os.TempDir()) + "/"}
	for _, d := range []string{".claude", ".codex", ".gemini", ".hermes", ".agy", ".antigravity", ".cursor"} {
		dirs = append(dirs, filepath.Join(home, d)+"/")
	}
	return dirs
}

func checkToolCommand(cmd string) error {
	first := strings.Fields(cmd)
	if len(first) == 0 {
		return errors.New("command is required")
	}
	p := first[0]
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, p[2:])
	}
	if !strings.Contains(p, "/") {
		return nil // an executable on PATH
	}
	abs, _ := filepath.Abs(p)
	for _, d := range privateDirs() {
		if strings.HasPrefix(abs+"/", d) || strings.HasPrefix(abs, d) {
			return fmt.Errorf("tool command %s is in a provider-private or temporary directory (%s); move it into .agent-shiftboard/tools/, a shared repository path, or ~/.config/agent-shiftboard/tools/ first", p, d)
		}
	}
	return nil
}

// RegisterTool registers or updates a shared tool.
func (c *Coordinator) RegisterTool(ctx context.Context, a Actor, t Tool) error {
	if err := checkName("tool", t.Name); err != nil {
		return err
	}
	if strings.TrimSpace(t.Description) == "" {
		return errors.New("a one-line description is required")
	}
	if err := vocab.ToolEffect.Check(t.Effect); err != nil {
		return err
	}
	if t.Status == "" {
		t.Status = "active"
	}
	if err := vocab.ToolStatus.Check(t.Status); err != nil {
		return err
	}
	if err := checkToolCommand(t.Command); err != nil {
		return err
	}
	for _, l := range t.Locks {
		if err := checkName("lock", l); err != nil {
			return err
		}
	}
	req, _ := json.Marshal(t.Requires)
	if err := secretscan.Check("tool", strings.Join([]string{t.Description, t.Command, t.Usage, t.Targets, string(req)}, " ")); err != nil {
		return err
	}
	db, err := c.resourceDB(a, t.Scope)
	if err != nil {
		return err
	}
	return store.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO tools(name,description,command,usage,effect_class,targets,requires,locks,created_by,verified_at,status,superseded_by,updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET description=excluded.description,command=excluded.command,usage=excluded.usage,effect_class=excluded.effect_class,
		 targets=excluded.targets,requires=excluded.requires,locks=excluded.locks,verified_at=excluded.verified_at,status=excluded.status,
		 superseded_by=excluded.superseded_by,updated_at=excluded.updated_at`,
			t.Name, t.Description, t.Command, t.Usage, t.Effect, t.Targets, string(req), strings.Join(t.Locks, ","),
			a.String(), t.VerifiedAt, t.Status, t.SupersededBy, c.now())
		return err
	})
}

// DeprecateTool marks a tool deprecated, or superseded by another tool.
func (c *Coordinator) DeprecateTool(ctx context.Context, a Actor, name, scope, supersededBy string) error {
	db, err := c.resourceDB(a, scope)
	if err != nil {
		return err
	}
	status := "deprecated"
	if supersededBy != "" {
		status = "superseded"
	}
	res, err := db.ExecContext(ctx, `UPDATE tools SET status=?,superseded_by=?,updated_at=? WHERE name=?`, status, supersededBy, c.now(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("tool %s not found in %s scope", name, scope)
	}
	return nil
}

// GetResources returns user and project resources (read operation).
func (c *Coordinator) GetResources(ctx context.Context) (Resources, error) {
	var r Resources
	for _, src := range []struct {
		db    *sql.DB
		scope string
	}{{c.UserDB, "user"}, {c.DB, "project"}} {
		rows, err := src.db.QueryContext(ctx, `SELECT name,value,description FROM config_values ORDER BY name`)
		if err != nil {
			return r, err
		}
		for rows.Next() {
			v := ConfigValue{Scope: src.scope}
			if err := rows.Scan(&v.Name, &v.Value, &v.Description); err != nil {
				rows.Close()
				return r, err
			}
			r.Config = append(r.Config, v)
		}
		rows.Close()
		rows, err = src.db.QueryContext(ctx, `SELECT name,location,keys,purpose,handling FROM secret_refs ORDER BY name`)
		if err != nil {
			return r, err
		}
		for rows.Next() {
			s := SecretRef{Scope: src.scope}
			var keys string
			if err := rows.Scan(&s.Name, &s.Location, &keys, &s.Purpose, &s.Handling); err != nil {
				rows.Close()
				return r, err
			}
			if keys != "" {
				s.Keys = strings.Split(keys, ",")
			}
			r.Secrets = append(r.Secrets, s)
		}
		rows.Close()
		rows, err = src.db.QueryContext(ctx, `SELECT name,description,command,usage,effect_class,targets,requires,locks,created_by,verified_at,status,superseded_by FROM tools ORDER BY name`)
		if err != nil {
			return r, err
		}
		for rows.Next() {
			t := Tool{Scope: src.scope}
			var req, locks string
			if err := rows.Scan(&t.Name, &t.Description, &t.Command, &t.Usage, &t.Effect, &t.Targets, &req, &locks, &t.CreatedBy, &t.VerifiedAt, &t.Status, &t.SupersededBy); err != nil {
				rows.Close()
				return r, err
			}
			_ = json.Unmarshal([]byte(req), &t.Requires)
			if locks != "" {
				t.Locks = strings.Split(locks, ",")
			}
			r.Tools = append(r.Tools, t)
		}
		rows.Close()
	}
	return r, nil
}

// Manifest is the declarative form applied with `agent-shiftboard apply -f`.
type Manifest struct {
	Scope   string                 `toml:"scope"`
	Config  map[string]ConfigValue `toml:"config"`
	Secrets map[string]SecretRef   `toml:"secrets"`
	Tools   map[string]Tool        `toml:"tools"`
}

// ApplyManifest applies a resource manifest (user operation).
func (c *Coordinator) ApplyManifest(ctx context.Context, a Actor, data []byte) (int, error) {
	if err := requireUser(a); err != nil {
		return 0, err
	}
	var m Manifest
	if _, err := toml.Decode(string(data), &m); err != nil {
		return 0, err
	}
	if m.Scope == "" {
		m.Scope = "project"
	}
	n := 0
	for _, name := range sortedKeys(m.Config) {
		v := m.Config[name]
		v.Name, v.Scope = name, m.Scope
		if err := c.SetConfig(ctx, a, v); err != nil {
			return n, fmt.Errorf("config.%s: %w", name, err)
		}
		n++
	}
	for _, name := range sortedKeys(m.Secrets) {
		s := m.Secrets[name]
		s.Name, s.Scope = name, m.Scope
		if err := c.AddSecretRef(ctx, a, s); err != nil {
			return n, fmt.Errorf("secrets.%s: %w", name, err)
		}
		n++
	}
	for _, name := range sortedKeys(m.Tools) {
		t := m.Tools[name]
		t.Name, t.Scope = name, m.Scope
		if err := c.RegisterTool(ctx, a, t); err != nil {
			return n, fmt.Errorf("tools.%s: %w", name, err)
		}
		n++
	}
	return n, nil
}

// ExportManifest renders one scope of the registry as a manifest.
func (c *Coordinator) ExportManifest(ctx context.Context, scope string) ([]byte, error) {
	r, err := c.GetResources(ctx)
	if err != nil {
		return nil, err
	}
	m := Manifest{Scope: scope, Config: map[string]ConfigValue{}, Secrets: map[string]SecretRef{}, Tools: map[string]Tool{}}
	for _, v := range r.Config {
		if v.Scope == scope {
			m.Config[v.Name] = v
		}
	}
	for _, s := range r.Secrets {
		if s.Scope == scope {
			m.Secrets[s.Name] = s
		}
	}
	for _, t := range r.Tools {
		if t.Scope == scope {
			m.Tools[t.Name] = t
		}
	}
	var buf bytes.Buffer
	err = toml.NewEncoder(&buf).Encode(m)
	return buf.Bytes(), err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
