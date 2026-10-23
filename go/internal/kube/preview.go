package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Request is used by the controller's preview provisioner with fixed API paths.
func (c *Client) Request(ctx context.Context, method, path string, in, out any) error {
	return c.do(ctx, method, path, in, out)
}

// ApplyPreview performs server-side apply, after the controller validates the object.
func (c *Client) ApplyPreview(ctx context.Context, namespace, group, resource, name string, object map[string]any) error {
	scoped := *c
	scoped.Namespace = namespace
	return scoped.do(ctx, http.MethodPatch, scoped.path(group, resource, name)+"?fieldManager=ai-pr-review-preview&force=true", object, nil)
}

func PreviewPath(namespace, resource, name string) string {
	p := fmt.Sprintf("/api/v1/namespaces/%s/%s", url.PathEscape(namespace), resource)
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}
