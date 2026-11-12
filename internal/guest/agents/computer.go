package agents

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Computer use: an MCP server named "computer" is registered for every
// harness. It is the Cua Driver when the image ships it, else our own small
// stdio server (`crowdvm-guest mcp-computer`) built on xdotool, ffmpeg, wmctrl
// and xclip against the sandbox desktop (DISPLAY :0).

const cuaDriver = "/opt/crowdvm/cua-driver/cua-driver"

type mcpCommand struct {
	Command string
	Args    []string
}

// computerServer returns how to start the computer MCP server, or nil when
// the sandbox has no desktop (minimal image).
func computerServer() *mcpCommand {
	if fileExists(cuaDriver) {
		if _, err := os.Stat("/run/ascii-cua/driver.sock"); err == nil {
			return &mcpCommand{cuaDriver, []string{"mcp", "--socket", "/run/ascii-cua/driver.sock"}}
		}
		return &mcpCommand{cuaDriver, []string{"mcp"}}
	}
	if _, err := exec.LookPath("Xvnc"); err != nil && os.Getenv("CROWDVM_COMPUTER_MCP") != "1" {
		return nil
	}
	if _, err := exec.LookPath("xdotool"); err != nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	return &mcpCommand{exe, []string{"mcp-computer"}}
}

func init() {
	// `crowdvm-guest mcp-computer`: serve MCP on stdio and exit.
	if len(os.Args) > 1 && os.Args[1] == "mcp-computer" {
		if err := ServeMCPComputer(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "mcp-computer:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

// ---------------------------------------------------------------------------
// Registration in each harness's own config (interactive use over SSH too).
// Integrated-agent runs also get it per run (flags / ACP session/new).
// ---------------------------------------------------------------------------

// registerComputer merges the computer server into the harness config files,
// leaving the user's other entries alone. Rewritten at every agent start.
func (m *Manager) registerComputer() {
	c := computerServer()
	if c == nil {
		return
	}
	entry := map[string]any{"command": c.Command, "args": c.Args}
	home := m.o.Home
	for _, f := range []struct{ path, key string }{
		{".claude.json", "mcpServers"},
		{".pi/agent/mcp.json", "mcpServers"},
		{".kimi-code/mcp.json", "mcpServers"},
		{".prime/agent/settings.json", "mcpServers"},
	} {
		m.mergeJSON(filepath.Join(home, f.path), func(cfg map[string]any) {
			servers, _ := cfg[f.key].(map[string]any)
			if servers == nil {
				servers = map[string]any{}
			}
			servers["computer"] = entry
			cfg[f.key] = servers
		})
	}
	m.mergeJSON(filepath.Join(home, ".config/opencode/opencode.json"), func(cfg map[string]any) {
		mcp, _ := cfg["mcp"].(map[string]any)
		if mcp == nil {
			mcp = map[string]any{}
		}
		mcp["computer"] = map[string]any{"type": "local", "command": append([]string{c.Command}, c.Args...)}
		cfg["mcp"] = mcp
		tools, _ := cfg["tools"].(map[string]any)
		if tools == nil {
			tools = map[string]any{}
		}
		tools["computer_browser_prepare"] = false
		cfg["tools"] = tools
		if cfg["$schema"] == nil {
			cfg["$schema"] = "https://opencode.ai/config.json"
		}
	})
	m.codexComputerBlock(c)
}

// mergeJSON edits a JSON object file in place (created when missing). A file
// that is not a JSON object is left alone.
func (m *Manager) mergeJSON(path string, edit func(map[string]any)) {
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
		if json.Unmarshal(b, &cfg) != nil {
			return
		}
	}
	edit(cfg)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	m.writeUserFile(path, append(b, '\n'))
}

const (
	codexBlockStart = "# >>> crowdvm computer-use (rewritten at every sandbox start) >>>"
	codexBlockEnd   = "# <<< crowdvm computer-use <<<"
)

// codexComputerBlock keeps a marked [mcp_servers.computer] block in
// ~/.codex/config.toml. A table the user defined outside the block wins.
func (m *Manager) codexComputerBlock(c *mcpCommand) {
	path := filepath.Join(m.o.Home, ".codex", "config.toml")
	b, _ := os.ReadFile(path)
	s := string(b)
	if i := strings.Index(s, codexBlockStart); i >= 0 {
		end := len(s)
		if j := strings.Index(s[i:], codexBlockEnd); j >= 0 {
			end = i + j + len(codexBlockEnd)
		}
		before, after := strings.TrimRight(s[:i], "\n"), strings.TrimLeft(s[end:], "\n")
		if before != "" && after != "" {
			before += "\n\n" // keep the text around the old block apart (bugs-r20-sonnet 8)
		}
		s = before + after
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
	}
	if strings.Contains(s, "[mcp_servers.computer]") {
		return
	}
	if s != "" {
		s += "\n"
	}
	s += codexBlockStart + "\n[mcp_servers.computer]\ncommand = " + tomlStr(c.Command) + "\nargs = " + tomlArr(c.Args) + "\n" + codexBlockEnd + "\n"
	m.writeUserFile(path, []byte(s))
}

// writeUserFile writes a file in the user's home, owned by the user.
func (m *Manager) writeUserFile(path string, data []byte) {
	cred := m.credential()
	dir := filepath.Dir(path)
	if !dirExists(dir) {
		// create missing parents one by one so each is owned by the user
		var missing []string
		for d := dir; !dirExists(d) && strings.HasPrefix(d, m.o.Home); d = filepath.Dir(d) {
			missing = append(missing, d)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			if os.Mkdir(missing[i], 0o755) == nil && cred != nil {
				_ = os.Chown(missing[i], int(cred.Uid), int(cred.Gid))
			}
		}
	}
	tmp := path + ".crowdvm.tmp"
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if os.WriteFile(tmp, data, mode) != nil {
		return
	}
	if cred != nil {
		_ = os.Chown(tmp, int(cred.Uid), int(cred.Gid))
	}
	if os.Rename(tmp, path) != nil {
		os.Remove(tmp)
	}
}

// ---------------------------------------------------------------------------
// The MCP server (protocol 2025-06-18, newline-delimited JSON-RPC on stdio)
// ---------------------------------------------------------------------------

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(a toolArgs) ([]any, error)
}

type toolArgs map[string]any

func (a toolArgs) str(k string) string { s, _ := a[k].(string); return s }

func (a toolArgs) int(k string) (int, bool) {
	switch v := a[k].(type) {
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	}
	return 0, false
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	pInt  = map[string]any{"type": "integer"}
	pStr  = map[string]any{"type": "string"}
	pXY   = map[string]any{"x": map[string]any{"type": "integer", "description": "screen x in pixels"}, "y": map[string]any{"type": "integer", "description": "screen y in pixels"}}
	pWin  = map[string]any{"type": "string", "description": "window id from list_windows (hex like 0x01e00003)"}
	pKeys = map[string]any{"type": "string", "description": "xdotool key names, e.g. Return, ctrl+l, alt+Tab"}
)

func merge(ms ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func textContent(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }

func computerTools() []mcpTool {
	click := func(button, repeat string) func(a toolArgs) ([]any, error) {
		return func(a toolArgs) ([]any, error) {
			args, err := moveArgs(a, false)
			if err != nil {
				return nil, err
			}
			return done(xdo(append(args, "click", "--repeat", repeat, button)...))
		}
	}
	return []mcpTool{
		{Name: "get_desktop_state", Description: "Screenshot of the whole desktop (JPEG) plus the list of open windows. Coordinates in the image are screen pixels.",
			InputSchema: schema(map[string]any{}), run: func(toolArgs) ([]any, error) { return desktopState("") }},
		{Name: "get_window_state", Description: "Screenshot of one window (JPEG) with its geometry. Clicks still use screen coordinates.",
			InputSchema: schema(map[string]any{"window_id": pWin}, "window_id"), run: func(a toolArgs) ([]any, error) { return desktopState(a.str("window_id")) }},
		{Name: "get_screen_size", Description: "Desktop size in pixels.", InputSchema: schema(map[string]any{}),
			run: func(toolArgs) ([]any, error) {
				out, err := xdo("getdisplaygeometry")
				return textContent(strings.Replace(strings.TrimSpace(out), " ", "x", 1)), err
			}},
		{Name: "list_windows", Description: "Open windows: id, desktop, x, y, width, height, title.", InputSchema: schema(map[string]any{}),
			run: func(toolArgs) ([]any, error) { s, err := listWindows(); return textContent(s), err }},
		{Name: "click", Description: "Left-click at x,y (or at the pointer).", InputSchema: schema(pXY), run: click("1", "1")},
		{Name: "double_click", Description: "Double-click at x,y.", InputSchema: schema(pXY), run: click("1", "2")},
		{Name: "right_click", Description: "Right-click at x,y.", InputSchema: schema(pXY), run: click("3", "1")},
		{Name: "move_cursor", Description: "Move the pointer to x,y.", InputSchema: schema(pXY, "x", "y"),
			run: func(a toolArgs) ([]any, error) {
				args, err := moveArgs(a, true)
				if err != nil {
					return nil, err
				}
				return done(xdo(args...))
			}},
		{Name: "drag", Description: "Drag with the left button from x,y to to_x,to_y.",
			InputSchema: schema(merge(pXY, map[string]any{"to_x": pInt, "to_y": pInt}), "x", "y", "to_x", "to_y"),
			run: func(a toolArgs) ([]any, error) {
				args, err := moveArgs(a, true)
				if err != nil {
					return nil, err
				}
				tx, ok1 := a.int("to_x")
				ty, ok2 := a.int("to_y")
				if !ok1 || !ok2 {
					return nil, fmt.Errorf("drag needs to_x and to_y")
				}
				return done(xdo(append(args, "mousedown", "1", "mousemove", "--sync", strconv.Itoa(tx), strconv.Itoa(ty), "mouseup", "1")...))
			}},
		{Name: "scroll", Description: "Scroll at x,y. direction: up|down|left|right; amount: wheel clicks (default 3).",
			InputSchema: schema(merge(pXY, map[string]any{"direction": map[string]any{"type": "string", "enum": []string{"up", "down", "left", "right"}}, "amount": pInt}), "direction"),
			run: func(a toolArgs) ([]any, error) {
				btn := map[string]string{"up": "4", "down": "5", "left": "6", "right": "7"}[a.str("direction")]
				if btn == "" {
					return nil, fmt.Errorf("direction must be up, down, left or right")
				}
				n, ok := a.int("amount")
				if !ok || n < 1 {
					n = 3
				}
				args, err := moveArgs(a, false)
				if err != nil {
					return nil, err
				}
				return done(xdo(append(args, "click", "--repeat", strconv.Itoa(n), btn)...))
			}},
		{Name: "type_text", Description: "Type text into the focused window.", InputSchema: schema(map[string]any{"text": pStr}, "text"),
			run: func(a toolArgs) ([]any, error) {
				return done(xdo("type", "--delay", "12", "--", a.str("text")))
			}},
		{Name: "press_key", Description: "Press keys (space separated sequence), e.g. \"Return\" or \"ctrl+l\".", InputSchema: schema(map[string]any{"key": pKeys}, "key"),
			run: func(a toolArgs) ([]any, error) {
				keys := strings.Fields(a.str("key"))
				if len(keys) == 0 {
					return nil, fmt.Errorf("key is required")
				}
				return done(xdo(append([]string{"key", "--"}, keys...)...))
			}},
		{Name: "hotkey", Description: "Press a key combination, e.g. keys [\"ctrl\",\"shift\",\"t\"].",
			InputSchema: schema(map[string]any{"keys": map[string]any{"type": "array", "items": pStr}}, "keys"),
			run: func(a toolArgs) ([]any, error) {
				var ks []string
				if arr, ok := a["keys"].([]any); ok {
					for _, k := range arr {
						if s, ok := k.(string); ok && s != "" {
							ks = append(ks, s)
						}
					}
				}
				if len(ks) == 0 {
					return nil, fmt.Errorf("keys is required")
				}
				return done(xdo("key", "--", strings.Join(ks, "+")))
			}},
		{Name: "launch_app", Description: "Start a program on the desktop, e.g. \"google-chrome https://example.com\" or \"xfce4-terminal\".",
			InputSchema: schema(map[string]any{"command": pStr}, "command"),
			run: func(a toolArgs) ([]any, error) {
				cmd := exec.Command("bash", "-lc", "exec "+a.str("command"))
				cmd.Stdout, cmd.Stderr = nil, nil
				setsid(cmd)
				if err := cmd.Start(); err != nil {
					return nil, err
				}
				go func() { _ = cmd.Wait() }()
				time.Sleep(time.Second)
				return textContent(fmt.Sprintf("started pid %d", cmd.Process.Pid)), nil
			}},
		{Name: "browser_navigate", Description: "Open a URL in Chrome (new tab in the running browser).", InputSchema: schema(map[string]any{"url": pStr}, "url"),
			run: func(a toolArgs) ([]any, error) {
				bin := "google-chrome"
				if _, err := exec.LookPath(bin); err != nil {
					bin = "xdg-open"
				}
				cmd := exec.Command(bin, a.str("url"))
				setsid(cmd)
				if err := cmd.Start(); err != nil {
					return nil, err
				}
				go func() { _ = cmd.Wait() }()
				time.Sleep(2 * time.Second)
				return textContent("opened " + a.str("url")), nil
			}},
		{Name: "bring_to_front", Description: "Activate and raise a window.", InputSchema: schema(map[string]any{"window_id": pWin}, "window_id"),
			run: func(a toolArgs) ([]any, error) { return done(run1("wmctrl", "-i", "-a", a.str("window_id"))) }},
		{Name: "set_window_frame", Description: "Move/resize a window.", InputSchema: schema(map[string]any{"window_id": pWin, "x": pInt, "y": pInt, "width": pInt, "height": pInt}, "window_id", "x", "y", "width", "height"),
			run: func(a toolArgs) ([]any, error) {
				x, _ := a.int("x")
				y, _ := a.int("y")
				w, _ := a.int("width")
				h, _ := a.int("height")
				return done(run1("wmctrl", "-i", "-r", a.str("window_id"), "-e", fmt.Sprintf("0,%d,%d,%d,%d", x, y, w, h)))
			}},
		{Name: "kill_app", Description: "Close a window gracefully (window_id) or kill processes by name (name).",
			InputSchema: schema(map[string]any{"window_id": pWin, "name": pStr}),
			run: func(a toolArgs) ([]any, error) {
				if id := a.str("window_id"); id != "" {
					return done(run1("wmctrl", "-i", "-c", id))
				}
				if n := a.str("name"); n != "" {
					return done(run1("pkill", "-x", n))
				}
				return nil, fmt.Errorf("window_id or name is required")
			}},
		{Name: "clipboard_read", Description: "Read the clipboard text.", InputSchema: schema(map[string]any{}),
			run: func(toolArgs) ([]any, error) {
				s, err := run1("xclip", "-selection", "clipboard", "-o")
				return textContent(s), err
			}},
		{Name: "clipboard_write", Description: "Set the clipboard text.", InputSchema: schema(map[string]any{"text": pStr}, "text"),
			run: func(a toolArgs) ([]any, error) {
				// xclip forks a child that serves the selection; it must not
				// inherit our pipes or Run would wait for it.
				cmd := exec.Command("xclip", "-selection", "clipboard", "-i")
				cmd.Stdin = strings.NewReader(a.str("text"))
				if err := cmd.Run(); err != nil {
					return nil, fmt.Errorf("xclip: %v", err)
				}
				return textContent("ok"), nil
			}},
	}
}

func done(_ string, err error) ([]any, error) {
	if err != nil {
		return nil, err
	}
	return textContent("ok"), nil
}

func moveArgs(a toolArgs, required bool) ([]string, error) {
	x, okx := a.int("x")
	y, oky := a.int("y")
	if !okx || !oky {
		if required {
			return nil, fmt.Errorf("x and y are required")
		}
		return nil, nil
	}
	return []string{"mousemove", "--sync", strconv.Itoa(x), strconv.Itoa(y)}, nil
}

func xdo(args ...string) (string, error) { return run1("xdotool", args...) }

func run1(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if strings.Contains(msg, "Can't open display") || strings.Contains(msg, "cannot open display") {
			msg = "the desktop (DISPLAY " + os.Getenv("DISPLAY") + ") is not running: " + msg
		}
		return out.String(), fmt.Errorf("%s: %v: %s", name, err, msg)
	}
	return out.String(), nil
}

func listWindows() (string, error) {
	if _, err := exec.LookPath("wmctrl"); err == nil {
		if out, err := run1("wmctrl", "-l", "-G"); err == nil {
			return out, nil
		} // no EWMH window manager (yet): ask X directly
	}
	ids, err := xdo("search", "--onlyvisible", "--name", ".")
	if err != nil && strings.TrimSpace(ids) != "" {
		return "", err
	} // exit 1 with no output = no windows
	var lines []string
	for _, id := range strings.Fields(ids) {
		name, _ := xdo("getwindowname", id)
		lines = append(lines, fmt.Sprintf("0x%08x %s", atoi(id), strings.TrimSpace(name)))
	}
	return strings.Join(lines, "\n"), nil
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// desktopState captures the screen (or one window's area) as JPEG.
func desktopState(window string) ([]any, error) {
	display := firstNonEmpty(os.Getenv("DISPLAY"), ":0")
	src, size, info := display, "", ""
	if window != "" {
		geo, err := xdo("getwindowgeometry", "--shell", window)
		if err != nil {
			return nil, err
		}
		g := map[string]int{}
		for _, l := range strings.Split(geo, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
				g[k] = atoi(v)
			}
		}
		if g["WIDTH"] <= 0 || g["HEIGHT"] <= 0 {
			return nil, fmt.Errorf("window %s has no size", window)
		}
		src = fmt.Sprintf("%s+%d,%d", display, max(g["X"], 0), max(g["Y"], 0))
		size = fmt.Sprintf("%dx%d", g["WIDTH"]&^1, g["HEIGHT"]&^1)
		info = fmt.Sprintf("window %s at x=%d y=%d size %dx%d (screen coordinates)", window, g["X"], g["Y"], g["WIDTH"], g["HEIGHT"])
	} else {
		if s, err := xdo("getdisplaygeometry"); err == nil {
			info = "screen " + strings.Replace(strings.TrimSpace(s), " ", "x", 1)
		}
		if w, err := listWindows(); err == nil {
			info += "\nwindows:\n" + strings.TrimSpace(w)
		}
	}
	var img []byte
	var err error
	if _, e := exec.LookPath("ffmpeg"); e == nil {
		args := []string{"-loglevel", "error", "-f", "x11grab"}
		if size != "" {
			args = append(args, "-video_size", size)
		}
		args = append(args, "-i", src, "-frames:v", "1", "-q:v", "4", "-f", "image2pipe", "-vcodec", "mjpeg", "-")
		img, err = exec.Command("ffmpeg", args...).Output()
	} else if _, e := exec.LookPath("import"); e == nil {
		target := "root"
		if window != "" {
			target = window
		}
		img, err = exec.Command("import", "-display", display, "-window", target, "-quality", "75", "jpeg:-").Output()
	} else {
		return nil, fmt.Errorf("no screenshot tool (ffmpeg or ImageMagick import) is installed")
	}
	if err != nil || len(img) == 0 {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("screenshot failed (is the desktop running on %s?): %v %s", display, err, msg)
	}
	return []any{map[string]any{"type": "image", "mimeType": "image/jpeg", "data": base64.StdEncoding.EncodeToString(img)},
		map[string]any{"type": "text", "text": info}}, nil
}

// ServeMCPComputer runs the computer MCP server on in/out until EOF.
func ServeMCPComputer(in io.Reader, out io.Writer) error {
	if os.Getenv("DISPLAY") == "" {
		os.Setenv("DISPLAY", ":0")
	}
	tools := computerTools()
	byName := map[string]mcpTool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	enc := json.NewEncoder(out)
	br := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var req rpcMsg
			if json.Unmarshal(line, &req) != nil {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			} else if len(req.ID) > 0 {
				result, rerr := mcpHandle(req, tools, byName)
				resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
				if rerr != nil {
					resp["error"] = rerr
				} else {
					resp["result"] = result
				}
				_ = enc.Encode(resp)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func mcpHandle(req rpcMsg, tools []mcpTool, byName map[string]mcpTool) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return map[string]any{"protocolVersion": firstNonEmpty(p.ProtocolVersion, "2025-06-18"),
			"capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo":   map[string]any{"name": "computer", "title": "CrowdVM computer use", "version": "1"},
			"instructions": "Tools to see and drive the sandbox's Linux desktop (DISPLAY :0, Xfce, Chrome). Take a screenshot with get_desktop_state, then click/type using screen pixel coordinates."}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string   `json:"name"`
			Arguments toolArgs `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		t, ok := byName[p.Name]
		if !ok {
			return nil, &rpcError{Code: -32602, Message: "unknown tool " + p.Name}
		}
		if p.Arguments == nil {
			p.Arguments = toolArgs{}
		}
		content, err := t.run(p.Arguments)
		if err != nil {
			return map[string]any{"content": textContent(err.Error()), "isError": true}, nil
		}
		return map[string]any{"content": content, "isError": false}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
}

// setsid detaches a launched desktop program from the MCP server's session.
func setsid(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
