package agentclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/greenbeanorg/skep/pkg/models"
)

// TemplateSources says what this host can build jail templates from.
func (c *Client) TemplateSources(ctx context.Context) (models.TemplateSources, error) {
	var src models.TemplateSources
	err := c.do(ctx, http.MethodGet, "/storage/template-sources", nil, nil, &src)
	return src, err
}

// Templates returns a page of jail templates.
func (c *Client) Templates(ctx context.Context, opts models.TemplateListOptions) (models.TemplateList, error) {
	q := url.Values{}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	var list models.TemplateList
	err := c.do(ctx, http.MethodGet, "/storage/templates", q, nil, &list)
	return list, err
}

// Template returns one jail template.
func (c *Client) Template(ctx context.Context, id string) (models.Template, error) {
	var t models.Template
	err := c.do(ctx, http.MethodGet, "/storage/templates/"+url.PathEscape(id), nil, nil, &t)
	return t, err
}

// CreateTemplate asks the agent to build a template for the caller in ctx.
func (c *Client) CreateTemplate(ctx context.Context, req models.TemplateCreate) (models.TemplateCreated, error) {
	var created models.TemplateCreated
	err := c.do(ctx, http.MethodPost, "/storage/templates", nil, req, &created)
	return created, err
}

// DeleteTemplate asks the agent to delete a template and returns the task.
func (c *Client) DeleteTemplate(ctx context.Context, id string) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodDelete, "/storage/templates/"+url.PathEscape(id), nil, nil, &task)
	return task, err
}
