package api

import (
	"errors"
	"html/template"
	"net/http"

	"github.com/kensac/orbit/internal/store"
)

// The pages are deliberately plain: they exist so a notification has
// somewhere to land before a real UI does. Everything they show comes from
// the same store methods the JSON API uses.

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>
<style>
:root{color-scheme:light dark;--fg:#1a1a1a;--muted:#666;--bg:#fafafa;--card:#fff;--line:#ddd;--ok:#1a7f37;--bad:#cf222e}
@media (prefers-color-scheme:dark){:root{--fg:#e6e6e6;--muted:#9a9a9a;--bg:#111;--card:#1b1b1b;--line:#333}}
body{font:16px/1.5 system-ui,sans-serif;color:var(--fg);background:var(--bg);margin:0;padding:16px}
main{max-width:760px;margin:0 auto}
.card{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:16px;margin:12px 0}
.muted{color:var(--muted);font-size:14px}
pre{white-space:pre-wrap;word-break:break-word;font:13px/1.45 ui-monospace,monospace;background:var(--bg);padding:12px;border-radius:6px;overflow-x:auto}
button{font:inherit;padding:10px 18px;border-radius:6px;border:1px solid var(--line);cursor:pointer;margin-right:8px}
.approve{background:var(--ok);color:#fff;border-color:var(--ok)}
.reject{background:var(--bad);color:#fff;border-color:var(--bad)}
</style></head><body><main>
<h1>{{.Title}}</h1>
{{if .Action}}{{with .Action}}
<div class="card">
<div class="muted">{{.Agent}} · proposed {{.CreatedAt.Format "2006-01-02 15:04 MST"}} · rule {{.Rule}} · status <b>{{.Status}}</b></div>
<h2>{{.Type}}</h2>
<p>{{.Reason}}</p>
<pre>{{printf "%s" .Params}}</pre>
{{if .Error}}<p class="muted">Error: {{.Error}}</p>{{end}}
</div>
{{end}}
{{if eq .Action.Status "pending"}}
<form method="post"><input type="hidden" name="sig" value="{{.Sig}}">
<button class="approve" name="decision" value="approve">Approve</button>
<button class="reject" name="decision" value="reject">Reject</button>
</form>{{end}}
{{end}}
{{if .Run}}{{with .Run}}
<div class="card">
<div class="muted">{{.Agent}} · {{.Trigger}} · {{.CreatedAt.Format "2006-01-02 15:04 MST"}} · <b>{{.Status}}</b>{{if .CostUSD}} · ${{printf "%.4f" .CostUSD}}{{end}}</div>
{{if .Summary}}<h2>{{.Summary}}</h2>{{end}}
{{if .Error}}<pre>{{.Error}}</pre>{{end}}
{{if .Report}}<pre>{{.Report}}</pre>{{end}}
</div>
{{end}}{{end}}
{{if .Message}}<p>{{.Message}}</p>{{end}}
</main></body></html>`))

type page struct {
	Title   string
	Action  *store.Action
	Run     *store.Run
	Sig     string
	Message string
}

func render(w http.ResponseWriter, status int, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, p)
}

func (s *Server) approvalPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sig := r.URL.Query().Get("sig")
	if !s.Engine.CheckApprovalSig(id, sig) {
		render(w, http.StatusForbidden, page{Title: "Link not valid"})
		return
	}
	a, err := s.Store.GetAction(r.Context(), id)
	if err != nil {
		render(w, http.StatusNotFound, page{Title: "Action not found"})
		return
	}
	render(w, http.StatusOK, page{Title: "Review action", Action: a, Sig: sig})
}

// approvalSubmit takes the decision. It is a POST from the page, never the
// GET a link preview or a prefetching mail client would make.
func (s *Server) approvalSubmit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil || !s.Engine.CheckApprovalSig(id, r.PostForm.Get("sig")) {
		render(w, http.StatusForbidden, page{Title: "Link not valid"})
		return
	}
	approve := r.PostForm.Get("decision") == "approve"
	err := s.Store.DecideAction(r.Context(), id, approve, "approval-link")
	a, _ := s.Store.GetAction(r.Context(), id)
	switch {
	case err == nil:
		s.Wake()
		msg := "Rejected."
		if approve {
			msg = "Approved. orbit will carry it out shortly."
		}
		render(w, http.StatusOK, page{Title: "Done", Action: a, Message: msg})
	case errors.Is(err, store.ErrConflict):
		render(w, http.StatusConflict, page{Title: "Already decided", Action: a})
	default:
		render(w, http.StatusInternalServerError, page{Title: "Error", Message: err.Error()})
	}
}

// runPage shows a run's report, read-only and without the API token: the
// tailnet bind is its boundary, as for the fleet's other tailnet UIs, and a
// run id is an unguessable UUID.
func (s *Server) runPage(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		render(w, http.StatusNotFound, page{Title: "Run not found"})
		return
	}
	render(w, http.StatusOK, page{Title: run.Agent + " run", Run: run})
}
