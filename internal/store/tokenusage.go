package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// TokenTotal is a sum of attempts' tokens (Tokens), how many attempts reported any, and how many runs those belong to. CacheHitRatio is cache_read over every prompt token, null when there were none.
type TokenTotal struct {
	Input         int64    `json:"input"`
	Output        int64    `json:"output"`
	CacheRead     int64    `json:"cache_read"`
	CacheCreation int64    `json:"cache_creation"`
	CacheHitRatio *float64 `json:"cache_hit_ratio"`
	Attempts      int      `json:"attempts"`
	Runs          int      `json:"runs"`
}

// AgentTokens is TokenTotal for one agent.
type AgentTokens struct {
	Agent string `json:"agent"`
	TokenTotal
}

// ModelTokens is TokenTotal for one model; "" is counts the backend named no model for.
type ModelTokens struct {
	Model string `json:"model"`
	TokenTotal
}

// DayTokens is TokenTotal for one calendar day.
type DayTokens struct {
	Date string `json:"date"`
	TokenTotal
}

// TokenReport is the tokens of runs created on the days from..to (inclusive, in tz).
type TokenReport struct {
	Total   TokenTotal    `json:"total"`
	ByAgent []AgentTokens `json:"by_agent"`
	ByModel []ModelTokens `json:"by_model"`
	ByDay   []DayTokens   `json:"by_day"`
}

// tokenSums are a TokenTotal's columns over attempts aliased a (and runs aliased r), in TokenTotal's order.
var tokenSums = `coalesce(sum(a.input_tokens), 0), coalesce(sum(a.output_tokens), 0),
	coalesce(sum(a.cache_read_tokens), 0), coalesce(sum(a.cache_creation_tokens), 0),
	` + hitRatioSQL("coalesce(sum(a.input_tokens), 0)", "coalesce(sum(a.cache_read_tokens), 0)", "coalesce(sum(a.cache_creation_tokens), 0)") + `::float8,
	count(a.input_tokens), count(DISTINCT a.run_id) FILTER (WHERE a.input_tokens IS NOT NULL)`

// runsInRange is runs aliased r created on the days $1..$2 in the zone $3.
const runsInRange = `r.created_at >= ($1::date)::timestamp AT TIME ZONE $3
	AND r.created_at < ($2::date + 1)::timestamp AT TIME ZONE $3`

// TokenUsage totals the tokens of runs created on the calendar days from..to (YYYY-MM-DD, inclusive) in the IANA zone tz: overall, per agent, per model and per day (every day listed, zero or not). An attempt counts on its run's day. One read-only snapshot, so the views agree.
func (s *Store) TokenUsage(ctx context.Context, from, to, tz string) (*TokenReport, error) {
	rep := &TokenReport{ByAgent: []AgentTokens{}, ByModel: []ModelTokens{}, ByDay: []DayTokens{}}
	scan := func(row pgx.CollectableRow, key *string, t *TokenTotal) error {
		dest := []any{&t.Input, &t.Output, &t.CacheRead, &t.CacheCreation, &t.CacheHitRatio, &t.Attempts, &t.Runs}
		if key != nil {
			dest = append([]any{key}, dest...)
		}
		return row.Scan(dest...)
	}
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT `+tokenSums+`
			FROM runs r LEFT JOIN run_attempts a ON a.run_id = r.id WHERE `+runsInRange, from, to, tz).
			Scan(&rep.Total.Input, &rep.Total.Output, &rep.Total.CacheRead, &rep.Total.CacheCreation,
				&rep.Total.CacheHitRatio, &rep.Total.Attempts, &rep.Total.Runs); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT r.agent, `+tokenSums+`
			FROM runs r JOIN run_attempts a ON a.run_id = r.id
			WHERE `+runsInRange+` AND a.input_tokens IS NOT NULL
			GROUP BY r.agent ORDER BY r.agent`, from, to, tz)
		if err != nil {
			return err
		}
		agents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AgentTokens, error) {
			var t AgentTokens
			return t, scan(row, &t.Agent, &t.TokenTotal)
		})
		if err != nil {
			return err
		}
		rep.ByAgent = append(rep.ByAgent, agents...)

		// An attempt's counts split by model when the backend split them, else all under its model.
		rows, err = tx.Query(ctx, `
			WITH m AS (
			    SELECT a.run_id, e.key AS model,
			           (e.value->>'input')::bigint AS input_tokens, (e.value->>'output')::bigint AS output_tokens,
			           (e.value->>'cache_read')::bigint AS cache_read_tokens, (e.value->>'cache_creation')::bigint AS cache_creation_tokens
			    FROM runs r JOIN run_attempts a ON a.run_id = r.id
			    CROSS JOIN LATERAL jsonb_each(coalesce(a.tokens_by_model, jsonb_build_object(coalesce(a.model, ''), jsonb_build_object(
			        'input', a.input_tokens, 'output', a.output_tokens,
			        'cache_read', a.cache_read_tokens, 'cache_creation', a.cache_creation_tokens)))) e
			    WHERE `+runsInRange+` AND a.input_tokens IS NOT NULL)
			SELECT a.model, `+tokenSums+` FROM m a GROUP BY a.model ORDER BY a.model`, from, to, tz)
		if err != nil {
			return err
		}
		models, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ModelTokens, error) {
			var t ModelTokens
			return t, scan(row, &t.Model, &t.TokenTotal)
		})
		if err != nil {
			return err
		}
		rep.ByModel = append(rep.ByModel, models...)

		rows, err = tx.Query(ctx, `
			SELECT d::date::text, `+tokenSums+`
			FROM generate_series($1::date, $2::date, interval '1 day') d
			LEFT JOIN (SELECT x.*, (r.created_at AT TIME ZONE $3)::date AS day
			           FROM runs r JOIN run_attempts x ON x.run_id = r.id
			           WHERE `+runsInRange+` AND x.input_tokens IS NOT NULL) a ON a.day = d::date
			GROUP BY d ORDER BY d`, from, to, tz)
		if err != nil {
			return err
		}
		days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DayTokens, error) {
			var t DayTokens
			return t, scan(row, &t.Date, &t.TokenTotal)
		})
		rep.ByDay = append(rep.ByDay, days...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// SkillUse is how often runs read a skill through orbit's skill tool (harness.ToolSkill): the runs that called it for the skill, the last call, and the versions those runs had (from their skills.json).
type SkillUse struct {
	Skill      string    `json:"skill"`
	Runs       int       `json:"runs"`
	LastUsedAt time.Time `json:"last_used_at"`
	Versions   []int     `json:"versions"`
}

// SkillUses lists, by skill name, the skills runs created on the days from..to (inclusive, in tz) read through the tool named tool. A call is counted from the journal (its first revision), so it is counted whether or not it succeeded.
func (s *Store) SkillUses(ctx context.Context, from, to, tz, tool string) ([]SkillUse, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT j.body->'input'->>'name', count(DISTINCT j.run_id), max(j.at),
		       coalesce(array_agg(DISTINCT v.version ORDER BY v.version) FILTER (WHERE v.version IS NOT NULL), '{}')
		FROM runs r
		JOIN journal_items j ON j.run_id = r.id AND j.kind = 'tool_call' AND j.revision = 1 AND j.body->>'name' = $4
		LEFT JOIN LATERAL (
		    SELECT (e->>'version')::integer AS version
		    FROM artifacts ar, jsonb_array_elements(convert_from(ar.content, 'UTF8')::jsonb) e
		    WHERE ar.run_id = r.id AND ar.attempt = j.attempt AND ar.direction = 'in' AND ar.name = 'skills.json'
		      AND e->>'name' = j.body->'input'->>'name') v ON true
		WHERE `+runsInRange+` AND coalesce(j.body->'input'->>'name', '') <> ''
		GROUP BY 1 ORDER BY 1`, from, to, tz, tool)
	if err != nil {
		return nil, err
	}
	uses, err := pgx.CollectRows(rows, pgx.RowToStructByPos[SkillUse])
	if uses == nil {
		uses = []SkillUse{}
	}
	return uses, err
}
