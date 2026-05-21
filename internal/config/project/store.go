package project

import "context"

// Project represents a configured project with its repository details.
type Project struct {
	Name          string
	RepoURL       string // shorthand like "org/repo" or full URL
	DefaultBranch string
	Forge         string // references forge config by name
}

// Store provides CRUD operations for projects.
type Store interface {
	Get(ctx context.Context, name string) (*Project, error)
	List(ctx context.Context) ([]*Project, error)
	Put(ctx context.Context, p *Project) error
	Delete(ctx context.Context, name string) error
}
