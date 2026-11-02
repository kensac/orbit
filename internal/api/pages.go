package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/kensac/orbit/internal/journal"
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
{{if .Timeline}}<div class="card"><div class="muted">Journal{{if gt .Epoch 0}} · timeline {{.Epoch}}{{end}}</div>
{{range .Timeline}}<div class="step" style="margin-left:{{.Indent}}em"><span class="muted">{{.Kind}}</span> <b>{{.Title}}</b>{{if .Detail}}<pre>{{.Detail}}</pre>{{end}}</div>
{{end}}</div>{{end}}
{{if .Message}}<p>{{.Message}}</p>{{end}}
</main></body></html>`))

type page struct {
	Title    string
	Action   *store.Action
	Run      *store.Run
	Sig      string
	Message  string
	Epoch    int
	Timeline []timelineRow
}

// timelineRow is one journal item as the run page shows it.
type timelineRow struct {
	Kind, Title, Detail string
	Indent              int
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

// runPage shows a run's report, read-only and without an API token:
// authentik forward-auth in front of /ui is its boundary, so /ui must never
// be routed around it.
func (s *Server) runPage(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		render(w, http.StatusNotFound, page{Title: "Run not found"})
		return
	}
	p := page{Title: run.Agent + " run", Run: run}
	if err := s.Store.SyncJournal(r.Context(), run.ID); err != nil {
		s.Log.Warn("run page journal sync", "run", run.ID, "err", err)
	}
	if h, err := s.Store.GetJournalHead(r.Context(), run.ID, 0); err == nil && h.Epoch > 0 {
		p.Epoch = h.Epoch
		var revs []journal.Item
		after := 0
		for {
			items, more, err := s.Store.ReadJournal(r.Context(), run.ID, h.Epoch, after, store.MaxJournalLimit)
			if err != nil {
				break
			}
			revs = append(revs, items...)
			if !more {
				break
			}
			after = items[len(items)-1].Seq
		}
		p.Timeline = timeline(journal.Fold(revs))
	}
	render(w, http.StatusOK, p)
}

// timeline renders folded journal items, a subagent's nested under the
// call that spawned it.
func timeline(items []journal.Item) []timelineRow {
	depth := map[string]int{}
	var out []timelineRow
	for _, it := range items {
		d := 0
		if it.Parent != nil {
			d = depth[*it.Parent] + 1
		}
		depth[it.ID] = d
		row := timelineRow{Kind: strings.ReplaceAll(it.Kind, "_", " "), Indent: 2 * d}
		switch it.Kind {
		case journal.KindMessage:
			var m journal.Message
			_ = json.Unmarshal(it.Body, &m)
			row.Title, row.Detail = m.Role, m.Text
		case journal.KindThinking:
			var t journal.Thinking
			_ = json.Unmarshal(it.Body, &t)
			row.Title, row.Detail = fmt.Sprintf("about %d tokens", t.TokensEstimate), t.Text.Head
		case journal.KindToolCall:
			var c journal.ToolCall
			_ = json.Unmarshal(it.Body, &c)
			row.Title = c.Name + " " + c.Summary + " (" + c.State + ")"
			if c.Result != nil {
				row.Detail = c.Result.Head
			}
		case journal.KindDiff:
			var d journal.Diff
			_ = json.Unmarshal(it.Body, &d)
			row.Title = d.Change + " " + d.Path
			if d.Patch != nil {
				row.Detail = d.Patch.Head
			}
		case journal.KindPlan:
			var p journal.Plan
			_ = json.Unmarshal(it.Body, &p)
			var b strings.Builder
			for _, e := range p.Entries {
				fmt.Fprintf(&b, "[%s] %s\n", e.Status, e.Text)
			}
			row.Title, row.Detail = plural(len(p.Entries), "step"), b.String()+p.Text
		case journal.KindApproval:
			var a journal.Approval
			_ = json.Unmarshal(it.Body, &a)
			row.Title, row.Detail = a.Tool+" ("+a.State+")", a.Message
		case journal.KindQuestion:
			var q journal.Question
			_ = json.Unmarshal(it.Body, &q)
			row.Title = q.Text
		case journal.KindStatus:
			var st journal.Status
			_ = json.Unmarshal(it.Body, &st)
			row.Title = st.Code
		case journal.KindTurn:
			var t journal.Turn
			_ = json.Unmarshal(it.Body, &t)
			row.Title = t.State
			if t.Outcome != "" {
				row.Title += ", " + t.Outcome
			}
			row.Detail = t.Error
		default:
			row.Detail = string(it.Body)
		}
		out = append(out, row)
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
