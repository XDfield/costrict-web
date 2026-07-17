// JWKS handler. Public endpoint — RFC 7517 explicitly permits JWKS to be
// served unauthenticated; public keys are not secrets.

package handlers

import (
	"net/http"

	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/gin-gonic/gin"
)

// JWKSAPI exposes cs-user's RSA signing key set. The receiver holds a
// (possibly nil) *auth.Signer so the route exists even when key loading is
// disabled — returns 503 with a descriptive error in that case, mirroring
// the unavailableUserService stub pattern.
type JWKSAPI struct {
	Signer *auth.Signer
}

// GetJWKS godoc
//
//	@Summary		JWKS endpoint
//	@Description	Exposes cs-user's RSA public signing key(s) so relying parties (server) can verify cs-user-issued JWTs. Public — RFC 7517 permits; public keys aren't secrets. Returns 503 when signing is not configured (CS_USER_JWT_SIGNING_KEY_PATH unset).
//	@Tags			jwks
//	@Produce		json
//	@Success		200	{object}	auth.JWKS
//	@Failure		503	{object}	object{error=string}
//	@Router			/.well-known/jwks [get]
func (a *JWKSAPI) GetJWKS(c *gin.Context) {
	if a.Signer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "JWKS not configured"})
		return
	}
	c.JSON(http.StatusOK, a.Signer.JWKS())
}
