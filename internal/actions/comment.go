package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/spec"
)

// CommentType posts a comment on an issue or a pull request as the App, in one of github.comment_repositories, with a token of issues:write alone, minted for the comment and revoked after it.
const CommentType = "github:comment"

type commentParams struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Body   string `json:"body"`
}

func commentType(d spec.Deployment) Type {
	return Type{
		Name: CommentType,
		Description: "Post a comment (GitHub markdown) on an issue or a pull request, as orbit's GitHub App. " +
			"It is public to everyone who can see the repository; write it for its readers.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo":   map[string]any{"type": "string", "enum": d.GitHub.CommentRepositories},
				"number": map[string]any{"type": "integer", "minimum": 1, "description": "the issue's or pull request's number"},
				"body":   map[string]any{"type": "string", "minLength": 1, "maxLength": github.MaxCommentBody},
			},
			"required":             []string{"repo", "number", "body"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			_, err := decodeComment(d, p)
			return err
		},
	}
}

func decodeComment(d spec.Deployment, p json.RawMessage) (commentParams, error) {
	var v commentParams
	if err := strictDecode(p, &v); err != nil {
		return v, err
	}
	if !d.AllowsComment(v.Repo) {
		return v, fmt.Errorf("github:comment: repository %q is not in github.comment_repositories", v.Repo)
	}
	if v.Number < 1 {
		return v, fmt.Errorf("github:comment: number must be positive")
	}
	if strings.TrimSpace(v.Body) == "" || len(v.Body) > github.MaxCommentBody {
		return v, fmt.Errorf("github:comment: body must be 1 to %d bytes", github.MaxCommentBody)
	}
	return v, nil
}

// comment mints an issues:write token for the one repository, posts, and revokes the token whatever happened.
func (e *Executor) comment(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeComment(e.Deployment, params)
	if err != nil {
		return nil, err
	}
	app, err := github.NewApp(e.Deployment.GitHub, e.Env, e.Client)
	if err != nil {
		return nil, err
	}
	tok, err := app.Token(ctx, p.Repo, github.CommentPermissions)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tok.Revoke(context.WithoutCancel(ctx)) }()
	c, err := tok.Client().CreateComment(ctx, p.Repo, p.Number, p.Body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"repo": p.Repo, "number": p.Number, "comment_id": c.ID, "url": c.HTMLURL})
}
