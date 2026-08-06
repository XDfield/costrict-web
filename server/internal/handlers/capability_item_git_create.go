// Package handlers — creating a capability whose content lives in Git.
//
// Cloud registers and discovers; Gitea is where capabilities are written (user
// decision U3). So "create" here means: provision a repository under the
// caller's own namespace with a manifest skeleton in it, register the index
// row, and hand back the repository coordinate for the client to send the user
// to. There is deliberately no in-place content editor — a second writer on
// content owned by a repository is the exact shape of drift this rollout
// removes.

package handlers

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// CreateGitBackedItem godoc
// @Summary      Create a Git-backed item
// @Description  Provision <short_id>/<slug> on the tenant's Gitea with a manifest skeleton, then register the capability as a Git-backed index row. The body of the capability is written in Gitea, not here — the response carries sourceRepoUrl / sourceRepoRef / sourceRepoPath for the hand-off.
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        body  body      object{itemType=string,slug=string,name=string,description=string,category=string,version=string,tags=[]string,author=string,license=string}  true  "Capability to create"
// @Success      201   {object}  ItemResponse
// @Failure      400   {object}  object{error=string,error_code=string}
// @Failure      401   {object}  object{error=string}
// @Failure      409   {object}  object{error=string,error_code=string}
// @Failure      502   {object}  object{error=string,error_code=string}
// @Failure      503   {object}  object{error=string,error_code=string}
// @Router       /items/git [post]
func (h *ItemHandler) CreateGitBackedItem(c *gin.Context) {
	var req struct {
		ItemType    string   `json:"itemType" binding:"required"`
		Slug        string   `json:"slug"`
		Name        string   `json:"name" binding:"required"`
		Description string   `json:"description"`
		Category    string   `json:"category"`
		Version     string   `json:"version"`
		Tags        []string `json:"tags"`
		Author      string   `json:"author"`
		License     string   `json:"license"`
		Content     string   `json:"content"`
		SourcePath  string   `json:"sourcePath"`
		Assets      []struct {
			RelPath     string `json:"relPath"`
			TextContent string `json:"textContent"`
		} `json:"assets"`
		Visibility string `json:"visibility"`
		RegistryID string `json:"registryId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	if req.Visibility != "" || req.RegistryID != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "visibility and registryId are not supported for git-backed creation", "error_code": "GIT_CREATE_FIELD_UNSUPPORTED"})
		return
	}

	userID := c.GetString(middleware.UserIDKey)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = slugify(req.Name)
	}
	version := firstNonEmpty(strings.TrimSpace(req.Version), "1.0.0")
	if len(req.Content) > 8*1024*1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content exceeds size limit", "error_code": "GIT_MANIFEST_TOO_LARGE"})
		return
	}
	manifestPath, ok := gitCapabilityManifestPath(req.ItemType)
	if !ok || (strings.TrimSpace(req.SourcePath) != "" && req.SourcePath != manifestPath) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sourcePath must match the canonical manifest path", "error_code": "GIT_SOURCE_PATH_INVALID"})
		return
	}
	extra := make([]GitCapabilityFile, 0, len(req.Assets))
	for _, asset := range req.Assets {
		if len(asset.TextContent) > 4*1024*1024 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "asset exceeds size limit", "error_code": "GIT_EXTRA_FILE_REJECTED"})
			return
		}
		extra = append(extra, GitCapabilityFile{Path: asset.RelPath, Content: []byte(asset.TextContent)})
	}

	resolvedTagIDs, err := resolveAssignableTags(h.tagSvc, req.Tags, userID, callerIsPlatformAdmin(c, h.db))
	if err != nil && errors.Is(err, services.ErrInvalidTagSlug) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Tag slug may only contain lowercase letters, numbers, hyphens, and underscores",
			"code":  "invalid_tag_slug",
		})
		return
	}

	// The repository comes first and is proven readable before anything is
	// written to the DB. The reverse order would publish a row whose content
	// address 404s, and nothing downstream repairs that.
	plan, herr := provisionGitCapabilityRepo(c.Request.Context(), resolveTenantID(c), userID, gitCapabilityProvisionSpec{
		ItemType:    req.ItemType,
		Slug:        slug,
		Name:        req.Name,
		Description: req.Description,
		Category:    req.Category,
		Version:     version,
		Tags:        req.Tags,
		Author:      req.Author,
		License:     req.License,
		Content:     req.Content,
		ExtraFiles:  extra,
	})
	if herr != nil {
		c.JSON(herr.status, herr.body)
		return
	}

	// Public registry, same as a fork: these rows are published alongside the
	// catalog and are told apart from it only by content_backend, which is why
	// the catalog ingest queries exclude that value rather than a registry.
	item, err := persistNewItem(h.db, createItemRequest{
		ID:          uuid.New().String(),
		RegistryID:  PublicRegistryID,
		RepoID:      registryRepoID(h.db, PublicRegistryID),
		Slug:        slug,
		ItemType:    req.ItemType,
		Name:        req.Name,
		Description: req.Description,
		Category:    req.Category,
		Version:     version,
		// No content column: the repository holds it and read-through serves it.
		ContentMD5: services.HashGitCapabilityContent(req.ItemType, plan.RepoPath, plan.Content),
		Metadata:   datatypes.JSON([]byte("{}")),
		SourcePath: plan.RepoPath,
		SourceType: "direct",
		CreatedBy:  userID,

		SourceRepoURL:     plan.RepoURL,
		SourceRepoRef:     plan.RepoRef,
		SourceRepoPath:    plan.RepoPath,
		ContentBackend:    contentBackendGit,
		SourceGitServerID: plan.GitServerID,
		SourceGitRepoID:   plan.GitRepoID,
		SourceGitEntryKey: plan.EntryKey,
		GitSyncStatus:     "pending",
	}, createItemAssets{})
	if err != nil {
		if errors.Is(err, ErrSlugConflict) {
			// The repository exists and is the user's; only the index row
			// collided. Renaming it here would decouple the row from the
			// repository name, so the caller picks another slug instead.
			c.JSON(http.StatusConflict, gin.H{
				"error":      "An item with this slug already exists",
				"error_code": "SLUG_CONFLICT",
				"slug":       slug,
				"repoUrl":    plan.RepoURL,
			})
			return
		}
		log.Printf("create git-backed item %s failed after provisioning %s: %v", slug, plan.RepoURL, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create item"})
		return
	}

	// Provisioning pushed one commit, but the push webhook is opt-in per
	// deployment and the row must not depend on it to leave 'pending'.
	enqueueForkGitSync(h.db, item.ID, plan)

	if h.categorySvc != nil && req.Category != "" {
		h.categorySvc.EnsureCategory(req.Category, userID)
	}
	if h.tagSvc != nil && len(resolvedTagIDs) > 0 {
		if err := assignTagsForItem(h.tagSvc, item.ID, resolvedTagIDs, services.TagSourceUser); err != nil {
			log.Printf("create git-backed item %s: assign tags failed: %v", item.ID, err)
		}
	}

	c.JSON(http.StatusCreated, buildItemResponse(c, h.db, *item, userID))
}
