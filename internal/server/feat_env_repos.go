package server

// Repositories for environments. Boat lists repositories of its GitHub App
// installations; CrowdVM lists them from git hosts the user configured with
// a token ("git sources": GitHub/GitHub Enterprise, Gitea/Forgejo, GitLab, or
// a plain git host credential). An installation = one account on a source.
// Any git URL reachable from the VMs can also be added directly.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

func (s *Server) envRepoRoutes() {
	s.API("GET /repos", "environment.read", s.handleReposList)
	s.API("POST /repos", "environment.write", s.handleReposSelect)
	s.API("GET /repos/sources", "account.read", s.handleGitSources)
	s.API("POST /repos/sources", "account.admin", s.handleGitSourceAdd)
	s.API("DELETE /repos/sources/{sourceId}", "account.admin", s.handleGitSourceDelete)
}

type gitSource struct {
	ID, OwnerID, Kind, Name, BaseURL, APIURL, Token string
	CreatedAt                                       time.Time
}

func (s *Server) gitSources(ownerID string) ([]*gitSource, error) {
	rows, err := s.DB.Query(`SELECT id,owner_id,kind,name,base_url,api_url,token,created_at FROM git_sources WHERE owner_id=? ORDER BY created_at`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*gitSource
	for rows.Next() {
		g := &gitSource{}
		var sealed string
		var created int64
		if err := rows.Scan(&g.ID, &g.OwnerID, &g.Kind, &g.Name, &g.BaseURL, &g.APIURL, &sealed, &created); err != nil {
			return nil, err
		}
		if b, err := s.envOpen(sealed); err == nil {
			g.Token = string(b)
		}
		g.CreatedAt = store.Time(created)
		out = append(out, g)
	}
	return out, rows.Err()
}

func gitHost(u string) string {
	if strings.HasPrefix(u, "git@") { // git@host:owner/repo.git
		h, _, _ := strings.Cut(strings.TrimPrefix(u, "git@"), ":")
		return strings.ToLower(h)
	}
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return strings.ToLower(p.Hostname())
}

// gitTokenFor returns the owner's token for a repository host ("" = none).
func (s *Server) gitTokenFor(ownerID, sourceID, cloneURL string) (string, *gitSource) {
	srcs, _ := s.gitSources(ownerID)
	for _, g := range srcs {
		if g.ID == sourceID {
			return g.Token, g
		}
	}
	h := gitHost(cloneURL)
	for _, g := range srcs {
		if h != "" && gitHost(g.BaseURL) == h {
			return g.Token, g
		}
	}
	return "", nil
}

func gitSourceJSON(g *gitSource) map[string]any {
	last := ""
	if len(g.Token) >= 4 {
		last = "…" + g.Token[len(g.Token)-4:]
	}
	return map[string]any{"id": g.ID, "kind": g.Kind, "name": g.Name, "baseUrl": g.BaseURL, "apiUrl": g.APIURL, "token": last, "createdAt": ts(g.CreatedAt)}
}

func (s *Server) handleGitSources(c *Ctx) error {
	srcs, err := s.gitSources(c.P.User.ID)
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, g := range srcs {
		out = append(out, gitSourceJSON(g))
	}
	return c.OK(200, "git_source.list", map[string]any{"sources": out})
}

func (s *Server) handleGitSourceAdd(c *Ctx) error {
	var req struct {
		Kind, Name, BaseURL, APIURL, Token string
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	req.Kind = firstNonEmpty(strings.ToLower(req.Kind), "github")
	req.BaseURL = strings.TrimRight(req.BaseURL, "/")
	req.APIURL = strings.TrimRight(req.APIURL, "/")
	switch req.Kind {
	case "github":
		req.BaseURL = firstNonEmpty(req.BaseURL, "https://github.com")
		if req.APIURL == "" {
			req.APIURL = "https://api.github.com"
			if gitHost(req.BaseURL) != "github.com" {
				req.APIURL = req.BaseURL + "/api/v3"
			}
		}
	case "gitlab":
		req.BaseURL = firstNonEmpty(req.BaseURL, "https://gitlab.com")
		req.APIURL = firstNonEmpty(req.APIURL, req.BaseURL+"/api/v4")
	case "gitea", "forgejo":
		req.Kind = "gitea"
		req.APIURL = firstNonEmpty(req.APIURL, req.BaseURL+"/api/v1")
	case "git":
	default:
		return Err(400, "invalid_kind", "kind must be github, gitlab, gitea or git.")
	}
	if u, err := url.Parse(req.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Err(400, "invalid_base_url", "baseUrl must be an http(s) URL of the git host.")
	}
	// bugs-r20-sonnet 1: same guard as webhooks and CI links, for both URLs.
	for _, raw := range []string{req.BaseURL, req.APIURL} {
		if raw == "" {
			continue
		}
		if err := s.checkOutboundURL(c.R.Context(), raw, "invalid_base_url"); err != nil {
			return err
		}
	}
	if strings.TrimSpace(req.Token) == "" || len(req.Token) > 8192 {
		return Err(400, "invalid_token", "token is required (a personal access token).")
	}
	sealed, err := s.envSeal([]byte(strings.TrimSpace(req.Token)))
	if err != nil {
		return err
	}
	g := &gitSource{ID: "gits_" + randToken(16), OwnerID: c.P.User.ID, Kind: req.Kind, Name: firstNonEmpty(req.Name, gitHost(req.BaseURL)),
		BaseURL: req.BaseURL, APIURL: req.APIURL, Token: strings.TrimSpace(req.Token), CreatedAt: time.Now().UTC()}
	if _, err := s.DB.Exec(`INSERT INTO git_sources(id,owner_id,kind,name,base_url,api_url,token,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		g.ID, g.OwnerID, g.Kind, g.Name, g.BaseURL, g.APIURL, sealed, store.T(g.CreatedAt)); err != nil {
		return err
	}
	return c.OK(201, "git_source.created", map[string]any{"source": gitSourceJSON(g)})
}

func (s *Server) handleGitSourceDelete(c *Ctx) error {
	res, err := s.DB.Exec(`DELETE FROM git_sources WHERE id=? AND owner_id=?`, c.Param("sourceId"), c.P.User.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound("Git source")
	}
	_, _ = s.DB.Exec(`DELETE FROM git_repos WHERE source_id=?`, c.Param("sourceId"))
	return c.OK(200, "git_source.deleted", map[string]any{"success": true})
}

// ---------------------------------------------------------------------------
// Listing repositories from git host APIs
// ---------------------------------------------------------------------------

type gitRepo struct {
	DatabaseID, SourceID, FullName, Name, Description, URL, CloneURL, Permissions string
	RepoID                                                                        int64
	Private                                                                       bool
	PushedAt                                                                      *time.Time
	AccountLogin, AccountType, AccountAvatar                                      string
}

// bugs-r20-sonnet 1: every dial (also after a redirect, the context carries the
// policy) goes through the outbound guard.
var gitHTTP = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: guardedDial, TLSHandshakeTimeout: 10 * time.Second}}

func gitGet(ctx context.Context, src *gitSource, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	switch src.Kind {
	case "gitlab":
		req.Header.Set("PRIVATE-TOKEN", src.Token)
	case "gitea":
		req.Header.Set("Authorization", "token "+src.Token)
	default:
		req.Header.Set("Authorization", "Bearer "+src.Token)
	}
	resp, err := gitHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		// bugs-r20-sonnet 1: status only; the remote body is not echoed.
		return fmt.Errorf("%s: HTTP %d", src.Name, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

func gitParseTime(v string) *time.Time {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t
	}
	return nil
}

// gitFetch lists the repositories a source's token can see.
func gitFetch(ctx context.Context, src *gitSource) ([]gitRepo, error) {
	var out []gitRepo
	short := strings.TrimPrefix(src.ID, "gits_")
	if len(short) > 6 {
		short = short[:6]
	}
	for page := 1; page <= 20; page++ {
		var n int
		switch src.Kind {
		case "github", "gitea":
			var list []struct {
				ID          int64  `json:"id"`
				Name        string `json:"name"`
				FullName    string `json:"full_name"`
				Private     bool   `json:"private"`
				Description string `json:"description"`
				HTMLURL     string `json:"html_url"`
				CloneURL    string `json:"clone_url"`
				PushedAt    string `json:"pushed_at"`
				UpdatedAt   string `json:"updated_at"`
				Permissions struct{ Admin, Push, Pull bool }
				Owner       struct {
					Login     string `json:"login"`
					Type      string `json:"type"`
					AvatarURL string `json:"avatar_url"`
				} `json:"owner"`
			}
			u := src.APIURL + "/user/repos?per_page=100&sort=pushed&page=" + fmt.Sprint(page)
			if src.Kind == "gitea" {
				u = src.APIURL + "/user/repos?limit=50&page=" + fmt.Sprint(page)
			}
			if err := gitGet(ctx, src, u, &list); err != nil {
				return out, err
			}
			n = len(list)
			for _, r := range list {
				perm := "read"
				if r.Permissions.Admin {
					perm = "admin"
				} else if r.Permissions.Push {
					perm = "write"
				}
				pushed := gitParseTime(firstNonEmpty(r.PushedAt, r.UpdatedAt))
				out = append(out, gitRepo{DatabaseID: fmt.Sprintf("repo_%s_%d", short, r.ID), SourceID: src.ID, RepoID: r.ID, FullName: r.FullName, Name: r.Name,
					Description: r.Description, URL: r.HTMLURL, CloneURL: r.CloneURL, Private: r.Private, Permissions: perm, PushedAt: pushed,
					AccountLogin: r.Owner.Login, AccountType: firstNonEmpty(r.Owner.Type, "User"), AccountAvatar: r.Owner.AvatarURL})
			}
		case "gitlab":
			var list []struct {
				ID           int64  `json:"id"`
				Name         string `json:"path"`
				FullName     string `json:"path_with_namespace"`
				Visibility   string `json:"visibility"`
				Description  string `json:"description"`
				WebURL       string `json:"web_url"`
				CloneURL     string `json:"http_url_to_repo"`
				LastActivity string `json:"last_activity_at"`
				Namespace    struct {
					FullPath  string `json:"full_path"`
					Kind      string `json:"kind"`
					AvatarURL string `json:"avatar_url"`
				} `json:"namespace"`
			}
			if err := gitGet(ctx, src, src.APIURL+"/projects?membership=true&per_page=100&order_by=last_activity_at&page="+fmt.Sprint(page), &list); err != nil {
				return out, err
			}
			n = len(list)
			for _, r := range list {
				typ := "User"
				if r.Namespace.Kind == "group" {
					typ = "Organization"
				}
				out = append(out, gitRepo{DatabaseID: fmt.Sprintf("repo_%s_%d", short, r.ID), SourceID: src.ID, RepoID: r.ID, FullName: r.FullName, Name: r.Name,
					Description: r.Description, URL: r.WebURL, CloneURL: r.CloneURL, Private: r.Visibility != "public", Permissions: "write",
					PushedAt: gitParseTime(r.LastActivity), AccountLogin: r.Namespace.FullPath, AccountType: typ, AccountAvatar: r.Namespace.AvatarURL})
			}
		default:
			return nil, nil
		}
		if n == 0 || (src.Kind != "gitea" && n < 100) || (src.Kind == "gitea" && n < 50) {
			break
		}
	}
	return out, nil
}

// gitSync refreshes the cached repository list of a user's sources.
func (s *Server) gitSync(ctx context.Context, ownerID string) error {
	srcs, err := s.gitSources(ownerID)
	if err != nil {
		return err
	}
	var firstErr error
	ctx = withOutboundPolicy(ctx, s.Cfg.WebhookAllowLocal) // bugs-r20-sonnet 1
	for _, src := range srcs {
		repos, err := gitFetch(ctx, src)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		now := store.Now()
		_, _ = s.DB.Exec(`DELETE FROM git_repos WHERE source_id=?`, src.ID)
		for _, r := range repos {
			_, _ = s.DB.Exec(`INSERT OR REPLACE INTO git_repos(database_id,source_id,owner_id,repo_id,full_name,name,description,url,clone_url,private,permissions,pushed_at,account_login,account_type,account_avatar,synced_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.DatabaseID, src.ID, ownerID, r.RepoID, r.FullName, r.Name, nz(r.Description), r.URL, r.CloneURL,
				boolInt(r.Private), r.Permissions, store.NullMs(r.PushedAt), r.AccountLogin, r.AccountType, r.AccountAvatar, now)
		}
	}
	return firstErr
}

func (s *Server) gitCached(ownerID string) ([]gitRepo, error) {
	rows, err := s.DB.Query(`SELECT database_id,source_id,repo_id,full_name,name,COALESCE(description,''),url,clone_url,private,permissions,pushed_at,account_login,account_type,account_avatar FROM git_repos WHERE owner_id=?`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []gitRepo
	for rows.Next() {
		var r gitRepo
		var pushed *int64
		if err := rows.Scan(&r.DatabaseID, &r.SourceID, &r.RepoID, &r.FullName, &r.Name, &r.Description, &r.URL, &r.CloneURL, &r.Private, &r.Permissions, &pushed, &r.AccountLogin, &r.AccountType, &r.AccountAvatar); err != nil {
			return nil, err
		}
		if pushed != nil {
			t := store.Time(*pushed)
			r.PushedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func gitRepoJSON(r gitRepo) map[string]any {
	var pushed any
	if r.PushedAt != nil {
		pushed = ts(*r.PushedAt)
	}
	return map[string]any{"databaseId": r.DatabaseID, "id": r.RepoID, "fullName": r.FullName, "name": r.Name, "description": nz(r.Description),
		"url": r.URL, "cloneUrl": r.CloneURL, "private": r.Private, "permissions": r.Permissions, "pushedAt": pushed, "lastUserCommitAt": nil, "sourceId": r.SourceID}
}

// envRepoFromURL builds a repository from a raw git URL or owner/name.
func envRepoFromURL(raw string) (envRepo, bool) {
	u := strings.TrimSpace(raw)
	if !(strings.Contains(u, "://") || strings.HasPrefix(u, "git@")) {
		return envRepo{}, false
	}
	p := u
	if strings.HasPrefix(u, "git@") {
		_, p, _ = strings.Cut(u, ":")
	} else if pu, err := url.Parse(u); err == nil && pu.Host != "" && (pu.Scheme == "https" || pu.Scheme == "http" || pu.Scheme == "ssh" || pu.Scheme == "git") {
		p = pu.Path
	} else {
		return envRepo{}, false
	}
	p = strings.Trim(strings.TrimSuffix(strings.TrimRight(p, "/"), ".git"), "/")
	if p == "" {
		return envRepo{}, false
	}
	name := p[strings.LastIndex(p, "/")+1:]
	sum := sha256.Sum256([]byte(u))
	web := strings.TrimSuffix(u, ".git")
	if strings.HasPrefix(u, "git@") {
		web = "https://" + gitHost(u) + "/" + p
	}
	return envRepo{RepositoryID: "git_" + hex.EncodeToString(sum[:6]), CloneURL: u, URL: web, FullName: gitHost(u) + "/" + p, Name: name, BaseBranch: "main"}, true
}

// envResolveRepo maps a repositoryId (databaseId from GET /repos, a git URL,
// or "owner/name" of a synced repository) to an environment repository.
func (s *Server) envResolveRepo(sc envScope, id string, cur []envRepo) (envRepo, error) {
	id = strings.TrimSpace(id)
	for _, r := range cur {
		if r.RepositoryID == id {
			return r, nil
		}
	}
	if r, ok := envRepoFromURL(id); ok {
		if _, src := s.gitTokenFor(sc.User.ID, "", r.CloneURL); src != nil {
			r.SourceID = src.ID
		}
		return r, nil
	}
	repos, _ := s.gitCached(sc.User.ID)
	if len(repos) == 0 {
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		_ = s.gitSync(ctx, sc.User.ID)
		cancel()
		repos, _ = s.gitCached(sc.User.ID)
	}
	for _, g := range repos {
		if g.DatabaseID == id || strings.EqualFold(g.FullName, id) || g.CloneURL == id {
			return envRepo{RepositoryID: g.DatabaseID, SourceID: g.SourceID, URL: g.URL, CloneURL: g.CloneURL, FullName: g.FullName, Name: g.Name,
				RepoID: g.RepoID, Private: g.Private, Description: g.Description, Permissions: g.Permissions, BaseBranch: "main"}, nil
		}
	}
	return envRepo{}, Err(400, "unknown_repository", "Unknown repository "+id+". Use a databaseId from GET /repos or a git URL.")
}

// ---------------------------------------------------------------------------
// GET/POST /repos (legacy: the default environment)
// ---------------------------------------------------------------------------

func (s *Server) handleReposList(c *Ctx) error {
	pg, err := ParsePage(c.R)
	if err != nil {
		return err
	}
	u := c.P.User
	repos, err := s.gitCached(u.ID)
	if err != nil {
		return err
	}
	var syncErr error
	if c.Query("sync") == "true" || len(repos) == 0 {
		ctx, cancel := context.WithTimeout(c.R.Context(), 60*time.Second)
		syncErr = s.gitSync(ctx, u.ID)
		cancel()
		if repos, err = s.gitCached(u.ID); err != nil {
			return err
		}
	}
	e, err := s.envDefault(u.ID, false)
	if err != nil {
		return err
	}
	v, err := s.envLatest(e)
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, r := range v.Config.Repos {
		selected[r.RepositoryID] = true
	}
	q := strings.ToLower(c.Query("q"))
	repos = slices.DeleteFunc(repos, func(r gitRepo) bool {
		if q != "" && !strings.Contains(strings.ToLower(r.FullName), q) && !strings.Contains(strings.ToLower(r.Name), q) {
			return true
		}
		if sel := c.Query("selected"); sel != "" && (sel == "true") != selected[r.DatabaseID] {
			return true
		}
		return false
	})
	key := func(r gitRepo) (int64, string) {
		if r.PushedAt == nil {
			return 0, r.DatabaseID
		}
		return r.PushedAt.UnixMilli(), r.DatabaseID
	}
	sort.Slice(repos, func(i, j int) bool {
		ti, ii := key(repos[i])
		tj, ij := key(repos[j])
		if ti != tj {
			return (ti < tj) == pg.Asc
		}
		return (ii < ij) == pg.Asc
	})
	if pg.HasCursor {
		repos = slices.DeleteFunc(repos, func(r gitRepo) bool {
			t, id := key(r)
			after := t < pg.AfterTime || (t == pg.AfterTime && id < pg.AfterID)
			if pg.Asc {
				after = t > pg.AfterTime || (t == pg.AfterTime && id > pg.AfterID)
			}
			return !after
		})
	}
	if len(repos) > pg.Limit+1 {
		repos = repos[:pg.Limit+1]
	}
	repos, info := PageResult(pg, repos, key)
	srcs, _ := s.gitSources(u.ID)
	srcName := map[string]string{}
	for _, g := range srcs {
		srcName[g.ID] = g.Name
	}
	var installs []map[string]any
	idx := map[string]int{}
	for _, r := range repos {
		k := r.SourceID + "|" + r.AccountLogin
		i, ok := idx[k]
		if !ok {
			i = len(installs)
			idx[k] = i
			installs = append(installs, map[string]any{"accountLogin": r.AccountLogin, "type": r.AccountType, "accountAvatarUrl": nz(r.AccountAvatar),
				"sourceId": r.SourceID, "source": srcName[r.SourceID], "repositories": []map[string]any{}})
		}
		installs[i]["repositories"] = append(installs[i]["repositories"].([]map[string]any), gitRepoJSON(r))
	}
	if installs == nil {
		installs = []map[string]any{}
	}
	out := map[string]any{"environmentId": v.ID, "subenvironmentId": e.ID, "installations": installs,
		"selectedRepositories": envReposJSON(v.Config.Repos), "pageInfo": pageJSON(info)}
	if syncErr != nil {
		out["syncError"] = syncErr.Error()
	}
	return c.OK(200, "repos.list", out)
}

func (s *Server) handleReposSelect(c *Ctx) error {
	var req envRepoReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.RepositoryID == "" {
		return Err(400, "invalid_repository", "repositoryId is required.")
	}
	u := c.P.User
	e, err := s.envDefault(u.ID, false)
	if err != nil {
		return err
	}
	cur, err := s.envLatest(e)
	if err != nil {
		return err
	}
	cfg := cur.Config
	cfg.Repos = slices.Clone(cfg.Repos)
	r, err := s.envResolveRepo(envScope{OwnerID: u.ID, User: u}, req.RepositoryID, cfg.Repos)
	if err != nil {
		return err
	}
	r.BaseBranch = firstNonEmpty(req.BaseBranch, "main")
	found := false
	for i := range cfg.Repos {
		if cfg.Repos[i].RepositoryID == r.RepositoryID {
			cfg.Repos[i].BaseBranch, found = r.BaseBranch, true
		}
	}
	if !found {
		cfg.Repos = append(cfg.Repos, r)
	}
	v, err := s.envMint(e, cfg)
	if err != nil {
		return err
	}
	return c.OK(200, "repos.updated", map[string]any{"success": true, "environmentId": v.ID, "subenvironmentId": e.ID, "selectedRepositories": envReposJSON(cfg.Repos)})
}
