// Quadlet Manager - web UI for managing podman Quadlet .container files.
//
// Environment variables:
//
//	QM_QUADLET_DIR  directory containing .container files (default: /etc/containers/systemd)
//	QM_LISTEN       listen address (default: 127.0.0.1:8600)
//	QM_USER_MODE    "1" to use `systemctl --user` (default: "0")
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

var (
	quadletDir = envOr("QM_QUADLET_DIR", "/etc/containers/systemd")
	listenAddr = envOr("QM_LISTEN", "127.0.0.1:8600")
	userMode   = os.Getenv("QM_USER_MODE") == "1"

	depDirectives  = []string{"Requires", "Wants", "BindsTo", "PartOf", "After", "Before"}
	allowedActions = map[string]bool{"start": true, "stop": true, "restart": true, "enable": true, "disable": true}
	targetRe       = regexp.MustCompile(`^[A-Za-z0-9@._-]+$`)
	sectionRe      = regexp.MustCompile(`^\[(.+)\]$`)
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func systemctl(args ...string) *exec.Cmd {
	if userMode {
		args = append([]string{"--user"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Cancel = func() error { cancel(); return cmd.Process.Kill() }
	return cmd
}

// ---------------------------------------------------------------- unit model

type Unit struct {
	File          string              `json:"file"`
	Service       string              `json:"service"`
	Description   string              `json:"description"`
	Image         string              `json:"image"`
	ContainerName string              `json:"container_name"`
	Networks      []string            `json:"networks"`
	Pod           string              `json:"pod"`
	Deps          map[string][]string `json:"deps"`
	WantedBy      []string            `json:"wanted_by"`
}

type Status struct {
	Active  string `json:"active"`
	Sub     string `json:"sub"`
	Enabled string `json:"enabled"`
	PID     string `json:"pid,omitempty"`
	Since   string `json:"since,omitempty"`
}

type kv struct{ key, value string }

func parseUnitFile(path string) (map[string][]kv, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sections := map[string][]kv{}
	current := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if m := sectionRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if current == "" {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			sections[current] = append(sections[current], kv{strings.TrimSpace(k), strings.TrimSpace(v)})
		}
	}
	return sections, nil
}

func values(sections map[string][]kv, section, key string) []string {
	var out []string
	for _, e := range sections[section] {
		if e.key == key {
			out = append(out, e.value)
		}
	}
	return out
}

func first(vals []string) string {
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func serviceName(fname string) string {
	return strings.TrimSuffix(fname, ".container") + ".service"
}

// unitPath resolves a .container filename inside quadletDir, refusing traversal.
func unitPath(fname string) (string, bool) {
	if strings.ContainsAny(fname, "/\\") || !strings.HasSuffix(fname, ".container") {
		return "", false
	}
	path := filepath.Join(quadletDir, fname)
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return "", false
	}
	return path, true
}

func collectUnits() ([]Unit, error) {
	entries, err := os.ReadDir(quadletDir)
	if err != nil {
		return nil, err
	}
	var units []Unit
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".container") {
			continue
		}
		sections, err := parseUnitFile(filepath.Join(quadletDir, e.Name()))
		if err != nil {
			continue
		}
		deps := map[string][]string{}
		for _, d := range depDirectives {
			var targets []string
			for _, v := range values(sections, "Unit", d) {
				targets = append(targets, strings.Fields(v)...)
			}
			if len(targets) > 0 {
				deps[d] = targets
			}
		}
		desc := strings.Join(values(sections, "Unit", "Description"), " ")
		if desc == "" {
			desc = e.Name()
		}
		nets := values(sections, "Container", "Network")
		if nets == nil {
			nets = []string{}
		}
		units = append(units, Unit{
			File:          e.Name(),
			Service:       serviceName(e.Name()),
			Description:   desc,
			Image:         first(values(sections, "Container", "Image")),
			ContainerName: first(values(sections, "Container", "ContainerName")),
			Networks:      nets,
			Pod:           first(values(sections, "Container", "Pod")),
			Deps:          deps,
			WantedBy:      values(sections, "Install", "WantedBy"),
		})
	}
	sort.Slice(units, func(i, j int) bool { return units[i].File < units[j].File })
	return units, nil
}

// ---------------------------------------------------------------- unit status

func unitStatuses(services []string) map[string]Status {
	statuses := map[string]Status{}
	for _, s := range services {
		statuses[s] = Status{Active: "unknown", Sub: "unknown", Enabled: "unknown"}
	}
	if len(services) == 0 {
		return statuses
	}
	args := append([]string{"show", "-p", "Id,ActiveState,SubState,UnitFileState,MainPID,StateChangeTimestamp"}, services...)
	out, err := systemctl(args...).Output()
	if err != nil && len(out) == 0 {
		return statuses
	}
	for _, block := range strings.Split(string(out), "\n\n") {
		props := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				props[k] = v
			}
		}
		if id := props["Id"]; id != "" {
			if _, known := statuses[id]; known {
				statuses[id] = Status{
					Active:  props["ActiveState"],
					Sub:     props["SubState"],
					Enabled: props["UnitFileState"],
					PID:     props["MainPID"],
					Since:   props["StateChangeTimestamp"],
				}
			}
		}
	}
	return statuses
}

// ---------------------------------------------------------------- graph model

type Node struct {
	ID           string   `json:"id"`
	Unit         Unit     `json:"unit"`
	Status       Status   `json:"status"`
	ExternalDeps []string `json:"external_deps"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

type Network struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Graph struct {
	QuadletDir   string    `json:"quadlet_dir"`
	UserMode     bool      `json:"user_mode"`
	Nodes        []Node    `json:"nodes"`
	Edges        []Edge    `json:"edges"`
	Networks     []Network `json:"networks"`
	NetworkEdges []Edge    `json:"network_edges"`
}

func buildGraph() (*Graph, error) {
	units, err := collectUnits()
	if err != nil {
		return nil, err
	}
	byService := map[string]string{}
	var services []string
	for _, u := range units {
		byService[u.Service] = u.File
		services = append(services, u.Service)
	}
	statuses := unitStatuses(services)

	g := &Graph{QuadletDir: quadletDir, UserMode: userMode,
		Nodes: []Node{}, Edges: []Edge{}, Networks: []Network{}, NetworkEdges: []Edge{}}
	netSeen := map[string]bool{}
	for _, u := range units {
		external := []string{}
		for _, d := range depDirectives {
			for _, t := range u.Deps[d] {
				if to, ok := byService[t]; ok {
					g.Edges = append(g.Edges, Edge{From: u.File, To: to, Type: d})
				} else if t != "network-online.target" && t != "local-fs.target" {
					external = append(external, d+"="+t)
				}
			}
		}
		for _, net := range u.Networks {
			name, _, _ := strings.Cut(net, ":")
			if !netSeen[name] {
				netSeen[name] = true
				g.Networks = append(g.Networks, Network{ID: "net:" + name, Name: name})
			}
			g.NetworkEdges = append(g.NetworkEdges, Edge{From: u.File, To: "net:" + name, Type: "Network"})
		}
		g.Nodes = append(g.Nodes, Node{ID: u.File, Unit: u, Status: statuses[u.Service], ExternalDeps: external})
	}
	sort.Slice(g.Networks, func(i, j int) bool { return g.Networks[i].ID < g.Networks[j].ID })
	return g, nil
}

// ---------------------------------------------------------------- file editing

// editDependency adds/removes target on directive lines in [Unit], preserving
// the rest of the file byte-for-byte (comments, order, spacing).
func editDependency(fname, directive, target, op string) error {
	valid := false
	for _, d := range depDirectives {
		valid = valid || d == directive
	}
	if !valid {
		return fmt.Errorf("unsupported directive: %s", directive)
	}
	if !targetRe.MatchString(target) {
		return fmt.Errorf("invalid target")
	}
	path, ok := unitPath(fname)
	if !ok {
		return fmt.Errorf("unit not found")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	unitStart, unitEnd := -1, len(lines)
	for i, line := range lines {
		s := strings.TrimSpace(line)
		if unitStart == -1 && s == "[Unit]" {
			unitStart = i
		} else if unitStart != -1 && sectionRe.MatchString(s) {
			unitEnd = i
			break
		}
	}
	directiveRe := regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(directive) + `\s*=\s*(.*?)\s*$`)

	switch {
	case unitStart == -1 && op == "add":
		lines = append([]string{"[Unit]\n", directive + "=" + target + "\n", "\n"}, lines...)
	case unitStart == -1:
		return fmt.Errorf("no [Unit] section")
	case op == "add":
		done := false
		for i := unitStart + 1; i < unitEnd; i++ {
			if m := directiveRe.FindStringSubmatch(lines[i]); m != nil {
				existing := strings.Fields(m[2])
				for _, t := range existing {
					if t == target {
						return fmt.Errorf("already present")
					}
				}
				lines[i] = m[1] + directive + "=" + strings.Join(append(existing, target), " ") + "\n"
				done = true
				break
			}
		}
		if !done {
			lines = append(lines[:unitEnd], append([]string{directive + "=" + target + "\n"}, lines[unitEnd:]...)...)
		}
	case op == "remove":
		removed := false
		for i := unitEnd - 1; i > unitStart; i-- {
			m := directiveRe.FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			var remaining []string
			for _, t := range strings.Fields(m[2]) {
				if t == target {
					removed = true
				} else {
					remaining = append(remaining, t)
				}
			}
			if !removed {
				continue
			}
			if len(remaining) > 0 {
				lines[i] = m[1] + directive + "=" + strings.Join(remaining, " ") + "\n"
			} else {
				lines = append(lines[:i], lines[i+1:]...)
			}
			break
		}
		if !removed {
			return fmt.Errorf("target not found on %s=", directive)
		}
	default:
		return fmt.Errorf("op must be add or remove")
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644)
}

func daemonReload() (bool, string) {
	out, err := systemctl("daemon-reload").CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		if msg == "" {
			msg = err.Error()
		}
		return false, msg
	}
	if msg == "" {
		msg = "ok"
	}
	return true, msg
}

// ---------------------------------------------------------------- HTTP server

func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(obj)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// pathUnit extracts and validates the {unit} segment from /api/units/{unit}/...
func pathUnit(w http.ResponseWriter, r *http.Request) (string, bool) {
	fname := r.PathValue("unit")
	if _, ok := unitPath(fname); !ok {
		writeErr(w, 404, "unit not found")
		return "", false
	}
	return fname, true
}

func main() {
	if fi, err := os.Stat(quadletDir); err != nil || !fi.IsDir() {
		log.Printf("warning: QM_QUADLET_DIR does not exist: %s", quadletDir)
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			writeErr(w, 500, "index.html missing")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	mux.HandleFunc("GET /api/graph", func(w http.ResponseWriter, r *http.Request) {
		g, err := buildGraph()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, g)
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		units, err := collectUnits()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		var services []string
		for _, u := range units {
			services = append(services, u.Service)
		}
		writeJSON(w, 200, unitStatuses(services))
	})

	mux.HandleFunc("GET /api/units/{unit}/file", func(w http.ResponseWriter, r *http.Request) {
		fname, ok := pathUnit(w, r)
		if !ok {
			return
		}
		path, _ := unitPath(fname)
		data, err := os.ReadFile(path)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "content": string(data)})
	})

	mux.HandleFunc("PUT /api/units/{unit}/file", func(w http.ResponseWriter, r *http.Request) {
		fname, ok := pathUnit(w, r)
		if !ok {
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		if readBody(r, &body) != nil || body.Content == "" {
			writeErr(w, 400, "missing content")
			return
		}
		if !strings.Contains(body.Content, "[Container]") {
			writeErr(w, 400, "refusing to save: no [Container] section")
			return
		}
		path, _ := unitPath(fname)
		if err := os.WriteFile(path, []byte(body.Content), 0o644); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		ok2, msg := daemonReload()
		writeJSON(w, 200, map[string]any{"ok": true, "daemon_reload": ok2, "reload_message": msg})
	})

	mux.HandleFunc("GET /api/units/{unit}/logs", func(w http.ResponseWriter, r *http.Request) {
		fname, ok := pathUnit(w, r)
		if !ok {
			return
		}
		n, err := strconv.Atoi(r.URL.Query().Get("lines"))
		if err != nil || n < 1 || n > 1000 {
			n = 80
		}
		args := []string{"-u", serviceName(fname), "-n", strconv.Itoa(n), "--no-pager", "-o", "short-iso"}
		if userMode {
			args = append([]string{"--user"}, args...)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "journalctl", args...).CombinedOutput()
		writeJSON(w, 200, map[string]any{"ok": err == nil, "logs": string(out)})
	})

	mux.HandleFunc("POST /api/units/{unit}/action", func(w http.ResponseWriter, r *http.Request) {
		fname, ok := pathUnit(w, r)
		if !ok {
			return
		}
		var body struct {
			Action string `json:"action"`
		}
		if readBody(r, &body) != nil || !allowedActions[body.Action] {
			writeErr(w, 400, "action must be one of start|stop|restart|enable|disable")
			return
		}
		out, err := systemctl(body.Action, serviceName(fname)).CombinedOutput()
		msg := strings.TrimSpace(string(out))
		if err != nil {
			if msg == "" {
				msg = err.Error()
			}
			writeJSON(w, 500, map[string]any{"ok": false, "message": msg})
			return
		}
		if msg == "" {
			msg = "ok"
		}
		writeJSON(w, 200, map[string]any{"ok": true, "message": msg})
	})

	mux.HandleFunc("POST /api/units/{unit}/deps", func(w http.ResponseWriter, r *http.Request) {
		fname, ok := pathUnit(w, r)
		if !ok {
			return
		}
		var body struct{ Op, Directive, Target string }
		if readBody(r, &body) != nil {
			writeErr(w, 400, "invalid body")
			return
		}
		if err := editDependency(fname, body.Directive, body.Target, body.Op); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		ok2, msg := daemonReload()
		writeJSON(w, 200, map[string]any{"ok": true, "message": "ok", "daemon_reload": ok2, "reload_message": msg})
	})

	mux.HandleFunc("POST /api/daemon-reload", func(w http.ResponseWriter, r *http.Request) {
		ok, msg := daemonReload()
		code := 200
		if !ok {
			code = 500
		}
		writeJSON(w, code, map[string]any{"ok": ok, "message": msg})
	})

	mode := "system"
	if userMode {
		mode = "user"
	}
	log.Printf("Quadlet Manager on http://%s  (dir: %s, %s mode)", listenAddr, quadletDir, mode)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
