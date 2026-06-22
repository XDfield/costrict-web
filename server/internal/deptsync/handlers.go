package deptsync

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Module wires the admin-facing dept-sync proxy endpoints. It depends on the
// dept-sync HTTP client (for the real tree/users) and a DB handle (to correlate
// dept-sync users back to local costrict-web users via universal id).
type Module struct {
	client *Client
	db     *gorm.DB
}

// NewModule builds the admin dept-sync module.
func NewModule(client *Client, db *gorm.DB) *Module {
	return &Module{client: client, db: db}
}

// RegisterRoutes mounts the dept-sync admin endpoints. The provided group is
// expected to already enforce platform-admin auth (e.g. main.go's /admin group),
// matching the adminuser/audit convention.
func (m *Module) RegisterRoutes(adminGroup *gin.RouterGroup) {
	adminGroup.GET("/departments/tree", m.GetTreeHandler())
	adminGroup.GET("/departments/:id/users", m.GetDeptUsersHandler())
}

// linkedUser is the costrict-web side of a dept-sync member, attached when the
// universal id matches a local user. nil-able fields stay empty for unregistered
// dept-sync members so the frontend can mark them "not registered".
type linkedUser struct {
	SubjectID    string   `json:"subjectId"`
	DisplayName  string   `json:"displayName"`
	Email        string   `json:"email"`
	AvatarURL    string   `json:"avatarUrl"`
	Organization string   `json:"organization"`
	Status       string   `json:"status"`
	Roles        []string `json:"roles"`
}

// deptMemberResponse is one row of a department's members: the dept-sync record
// plus the correlated local user (Registered=false when no local user matched).
type deptMemberResponse struct {
	UserID      string      `json:"userId"`
	Username    string      `json:"username"`
	UniversalID string      `json:"universalId"`
	IsMain      bool        `json:"isMain"`
	Position    string      `json:"position"`
	Registered  bool        `json:"registered"`
	Linked      *linkedUser `json:"linked,omitempty"`
}

// GetTreeHandler godoc
//
//	@Summary		Department tree (admin)
//	@Description	Proxy the real dept-sync department tree (platform admin only). Returns 503 when dept-sync is not configured/unreachable.
//	@Tags			admin/departments
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	object{departments=[]object}
//	@Failure		401	{object}	object{error=string}
//	@Failure		503	{object}	object{error=string}
//	@Router			/admin/departments/tree [get]
func (m *Module) GetTreeHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		tree, err := m.client.GetTree()
		if err != nil {
			respondDeptSyncError(c, err)
			return
		}
		if tree == nil {
			tree = []Dept{}
		}
		c.JSON(http.StatusOK, gin.H{"departments": tree})
	}
}

// GetDeptUsersHandler godoc
//
//	@Summary		Department members (admin)
//	@Description	Members of one department from dept-sync, correlated to local users via universal id (platform admin only). Returns 503 when dept-sync is not configured/unreachable.
//	@Tags			admin/departments
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		string	true	"Department id (dept_id)"
//	@Success		200	{object}	object{members=[]object}
//	@Failure		401	{object}	object{error=string}
//	@Failure		503	{object}	object{error=string}
//	@Router			/admin/departments/{id}/users [get]
func (m *Module) GetDeptUsersHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		deptID := c.Param("id")
		users, err := m.client.GetDeptUsers(deptID)
		if err != nil {
			respondDeptSyncError(c, err)
			return
		}

		out := m.correlate(users)
		c.JSON(http.StatusOK, gin.H{"members": out})
	}
}

// correlate enriches each dept-sync member with its local user (matched by
// universal id). dept-sync members with no local match are returned with
// Registered=false so the UI can render them but disable "open detail".
func (m *Module) correlate(users []DeptUser) []deptMemberResponse {
	out := make([]deptMemberResponse, 0, len(users))
	if len(users) == 0 {
		return out
	}

	// Collect non-empty universal ids to batch-load matching local users.
	uids := make([]string, 0, len(users))
	seen := make(map[string]struct{}, len(users))
	for _, u := range users {
		if u.UniversalID == "" {
			continue
		}
		if _, ok := seen[u.UniversalID]; ok {
			continue
		}
		seen[u.UniversalID] = struct{}{}
		uids = append(uids, u.UniversalID)
	}

	byUID := m.usersByUniversalID(uids)

	// Batch-load roles for all matched local users in one query.
	subjectIDs := make([]string, 0, len(byUID))
	for _, lu := range byUID {
		subjectIDs = append(subjectIDs, lu.SubjectID)
	}
	rolesBySubject := m.rolesForUsers(subjectIDs)

	for _, u := range users {
		row := deptMemberResponse{
			UserID:      u.UserID,
			Username:    u.Username,
			UniversalID: u.UniversalID,
			IsMain:      u.IsMain,
			Position:    u.Position,
		}
		if local, ok := byUID[u.UniversalID]; ok && u.UniversalID != "" {
			roles := rolesBySubject[local.SubjectID]
			if roles == nil {
				roles = []string{}
			}
			row.Registered = true
			row.Linked = &linkedUser{
				SubjectID:    local.SubjectID,
				DisplayName:  derefStr(local.DisplayName),
				Email:        derefStr(local.Email),
				AvatarURL:    derefStr(local.AvatarURL),
				Organization: derefStr(local.Organization),
				Status:       statusOrDefault(local.Status),
				Roles:        roles,
			}
		}
		out = append(out, row)
	}
	return out
}

// usersByUniversalID batch-loads local users whose casdoor_universal_id is in the
// given set, keyed by universal id. Returns an empty map on any DB issue or empty
// input (the endpoint still returns the dept-sync rows, just unlinked).
func (m *Module) usersByUniversalID(uids []string) map[string]models.User {
	out := make(map[string]models.User, len(uids))
	if len(uids) == 0 || m.db == nil {
		return out
	}
	var rows []models.User
	if err := m.db.Where("casdoor_universal_id IN ?", uids).Find(&rows).Error; err != nil {
		return out
	}
	for _, u := range rows {
		if u.CasdoorUniversalID != nil {
			out[*u.CasdoorUniversalID] = u
		}
	}
	return out
}

// rolesForUsers batch-loads system roles for the given subject ids in one query.
func (m *Module) rolesForUsers(subjectIDs []string) map[string][]string {
	out := make(map[string][]string, len(subjectIDs))
	if len(subjectIDs) == 0 || m.db == nil {
		return out
	}
	type roleRow struct {
		UserID string
		Role   string
	}
	var rows []roleRow
	m.db.Model(&models.UserSystemRole{}).
		Select("user_id, role").
		Where("user_id IN ? AND deleted_at IS NULL", subjectIDs).
		Order("created_at ASC").
		Scan(&rows)
	for _, r := range rows {
		out[r.UserID] = append(out[r.UserID], r.Role)
	}
	return out
}

// respondDeptSyncError maps a dept-sync client error to an HTTP response. A
// not-configured/unreachable dept-sync degrades to 503 with a stable error code
// the frontend keys on to show the "department service unavailable" notice.
func respondDeptSyncError(c *gin.Context, err error) {
	if errors.Is(err, ErrNotConfigured) {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "department service is not configured",
			"code":  "dept_sync_unavailable",
		})
		return
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": "department service is unavailable",
		"code":  "dept_sync_unavailable",
	})
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func statusOrDefault(s string) string {
	if s == "" {
		return "active"
	}
	return s
}
