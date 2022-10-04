package bookkeeper

import (
	"context"

	api "github.com/akuityio/k8sta/api/v1alpha1"
	"github.com/akuityio/k8sta/internal/git"
)

// Request is a request for Bookkeeper to render some environment-specific
// configuration from the repository specified by RepoURL into plain YAML in an
// environment-specific branch.
type Request struct {
	// RepoURL is the URL of a remote GitOps repository.
	RepoURL string `json:"repoURL,omitempty"`
	// RepoCreds encapsulates read/write credentials for the remote GitOps
	// repository referenced by the RepoURL field.
	RepoCreds git.RepoCredentials `json:"repoCreds,omitempty"`
	// Path is the path to a directory in the GitOps repository referenced by the
	// RepoURL field which contains environment-specific configuration.
	Path string `json:"path,omitempty"`
	// TargetBranch is the name of an environment-specific branch in the GitOps
	// repository referenced by the RepoURL field into which plain YAML should be
	// rendered.
	TargetBranch string `json:"targetBranch,omitempty"`
	// ConfigManagement encapsulates details of which configuration management
	// tool is to be used and, if applicable, configuration options for the
	// selected tool.
	ConfigManagement api.ConfigManagementConfig `json:"configManagement,omitempty"` // nolint: lll
}

// Response encapsulates details of a successful rendering of some some
// environment-specific configuration into plain YAML in an environment-specific
// branch.
type Response struct {
	// CommitID is the ID (sha) of the commit to the environment-specific branch
	// containing the rendered configuration.
	CommitID string `json:"commitID,omitempty"`
}

// Service is an interface for components that can handle bookkeeping requests.
// Implementations of this interface are transport-agnostic.
type Service interface {
	// Handle handles a bookkeeping request.
	Handle(context.Context, Request) (Response, error)
}