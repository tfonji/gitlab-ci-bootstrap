// Package gitlabclient wraps the upstream GitLab REST client.
package gitlabclient

import (
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

type Client struct {
	REST *gitlab.Client
}

func New(baseURL, token string) (*Client, error) {
	rest, err := gitlab.NewClient(token, gitlab.WithBaseURL(baseURL))
	if err != nil {
		return nil, err
	}
	return &Client{REST: rest}, nil
}
