package gitea

import (
	"time"

	"code.gitea.io/sdk/gitea"

	"github.com/MichaelMure/git-bug/bridge/core"
	"github.com/MichaelMure/git-bug/bridge/core/auth"
)

const (
	target = "gitea"

	metaKeyGiteaId      = "gitea-id"
	metaKeyGiteaUrl     = "gitea-url"
	metaKeyGiteaLogin   = "gitea-login"
	metaKeyGiteaBaseUrl = "gitea-base-url"

	confKeyOwner        = "owner"
	confKeyProject      = "project"
	confKeyGiteaBaseUrl = "base-url"
	confKeyDefaultLogin = "default-login"

	defaultBaseURL = "https://gitea.com/"
	defaultTimeout = time.Minute
)

var _ core.BridgeImpl = &Gitea{}

type Gitea struct{}

func (Gitea) Target() string {
	return target
}

func (g *Gitea) LoginMetaKey() string {
	return metaKeyGiteaLogin
}

func (Gitea) NewImporter() core.Importer {
	return &giteaImporter{}
}

func (Gitea) NewExporter() core.Exporter {
	return &giteaExporter{}
}

func buildClient(baseURL string, token *auth.Token) (*gitea.Client, error) {
	return gitea.NewClient(baseURL, gitea.SetToken(token.Value))
}
