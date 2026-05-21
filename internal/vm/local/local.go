//go:build !darwin && !linux

package local

import (
	"context"
	"errors"

	"github.com/aholstenson/kvarn/internal/vm"
)

type Provider struct{}

func (p *Provider) Name() string { return "local" }

func (p *Provider) PrepareImage(_ context.Context, _ vm.BaseImage) (*vm.ProviderImage, error) {
	return nil, errors.ErrUnsupported
}

func (p *Provider) Create(_ context.Context, _ vm.CreateOpts) (*vm.VM, *vm.RunnerConn, error) {
	return nil, nil, errors.ErrUnsupported
}

func (p *Provider) Destroy(_ context.Context, _ string) error {
	return errors.ErrUnsupported
}

func (p *Provider) List(_ context.Context) ([]*vm.VM, error) {
	return nil, errors.ErrUnsupported
}

// NewProvider creates a new Provider for unsupported platforms.
// All operations return errors.ErrUnsupported.
func NewProvider() *Provider { return &Provider{} }
