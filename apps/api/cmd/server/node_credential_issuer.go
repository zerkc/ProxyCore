package main

import (
	"context"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	nodesync "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

// nodeCredentialIssuer is the production enrollment.NodeCredentialIssuer. It
// lives in cmd/server because internal/sync tests import internal/enrollment,
// so enrollment cannot import sync without an import cycle.
type nodeCredentialIssuer struct{}

func (nodeCredentialIssuer) IssueNodeCredential(ctx context.Context) (enrollment.NodeCredentialMaterial, error) {
	if ctx == nil || ctx.Err() != nil {
		return enrollment.NodeCredentialMaterial{}, enrollment.ErrEnrollmentDenied
	}
	credential, err := nodesync.GenerateNodeCredential()
	if err != nil {
		return enrollment.NodeCredentialMaterial{}, enrollment.ErrEnrollmentDenied
	}
	defer credential.Destroy()
	return enrollment.NodeCredentialMaterial{
		ID:          credential.ID(),
		Secret:      credential.CopyBytes(),
		Hash:        credential.Hash(),
		HashVersion: nodesync.NodeCredentialHashVersion,
	}, nil
}

func (nodeCredentialIssuer) ValidateNodeCredential(material enrollment.NodeCredentialMaterial) error {
	if material.HashVersion != nodesync.NodeCredentialHashVersion {
		return enrollment.ErrEnrollmentDenied
	}
	credential, err := nodesync.NewNodeCredential(material.ID, material.Secret)
	if err != nil {
		return enrollment.ErrEnrollmentDenied
	}
	defer credential.Destroy()
	if credential.Hash() != material.Hash {
		return enrollment.ErrEnrollmentDenied
	}
	return nil
}
