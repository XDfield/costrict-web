// Package handlers — serving Git-backed capability content.
//
// Three endpoints return a capability's content: the item detail, the item
// download, and the registry file download. For a Git-backed row all three read
// through to the repository on every request (services.GitCapabilityContentService);
// none of them may answer from capability_items.content.
//
// The list endpoint deliberately does NOT read through — see the comment at its
// blanking site in ListAllItems.
//
// Failure is reported, never papered over. Returning the stored column when Git
// is unreachable would hand back content of unknown age while looking like a
// success, and returning an empty body would look like an empty capability. Both
// are worse than a status code the caller can act on.

package handlers

import (
	"errors"
	"log"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// gitContentSvcOverride replaces the default service in tests that need to
// drive the Git edge directly. Production leaves it nil: the service is built
// per request from the current DB handle, the same way every other Git call in
// this package builds its client.
var gitContentSvcOverride *services.GitCapabilityContentService

func gitContentService() *services.GitCapabilityContentService {
	if gitContentSvcOverride != nil {
		return gitContentSvcOverride
	}
	return services.NewGitCapabilityContentService(database.GetDB())
}

// readGitBackedItemContent fetches the item's content from its repository.
// Callers must have established that the item is Git-backed.
func readGitBackedItemContent(c *gin.Context, item *models.CapabilityItem) ([]byte, *httpErr) {
	raw, err := gitContentService().ItemContentBytes(c.Request.Context(), item)
	if err != nil {
		return nil, gitContentHTTPError(item, err)
	}
	return raw, nil
}

// gitContentHTTPError maps a read-through failure onto a response.
//
// The underlying error is logged rather than returned: it can carry the git
// server's internal address, and these endpoints answer anonymous callers. The
// error_code plus the repository coordinate (already public in the detail
// response) is what a caller needs to tell "the upstream is down" from "this
// item does not exist" — the distinction the status code alone cannot make.
func gitContentHTTPError(item *models.CapabilityItem, err error) *httpErr {
	log.Printf("git content read-through failed for item %s (%s@%s:%s): %v",
		item.ID, item.SourceRepoURL, item.SourceRepoRef, item.SourceRepoPath, err)

	body := func(message, code string) gin.H {
		return gin.H{
			"error":      message,
			"error_code": code,
			"repoUrl":    item.SourceRepoURL,
			"repoRef":    item.SourceRepoRef,
			"repoPath":   item.SourceRepoPath,
		}
	}

	switch {
	case errors.Is(err, services.ErrGitContentMissing):
		return &httpErr{
			status: http.StatusBadGateway,
			body: body("this item's file no longer exists in its git repository; restore it in the repository or archive the item",
				"GIT_CONTENT_MISSING"),
		}
	case errors.Is(err, services.ErrGitContentForbidden):
		return &httpErr{
			status: http.StatusBadGateway,
			body: body("the git server refused to serve this item's content; contact your platform admin",
				"GIT_CONTENT_FORBIDDEN"),
		}
	case errors.Is(err, services.ErrGitContentCoordinate):
		return &httpErr{
			status: http.StatusBadGateway,
			body: body("this item's repository coordinate is incomplete, so its content cannot be located; contact your platform admin",
				"GIT_CONTENT_COORDINATE_INVALID"),
		}
	case errors.Is(err, services.ErrGitContentServer):
		return &httpErr{
			status: http.StatusServiceUnavailable,
			body: body("the git server holding this item is unavailable; try again later",
				"GIT_CONTENT_SERVER_UNAVAILABLE"),
		}
	default:
		return &httpErr{
			status: http.StatusBadGateway,
			body: body("this item's content is stored in git and the git server did not respond; try again later",
				"GIT_CONTENT_UNREACHABLE"),
		}
	}
}
