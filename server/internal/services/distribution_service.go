package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/deptsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DistributionService handles item distribution (push/share) logic.
type DistributionService struct {
	db              *gorm.DB
	behaviorSvc     *BehaviorService
	notificationSvc NotificationSender
	deptSync        deptMemberResolver
}

// NotificationSender abstracts the notification service for distribution events.
type NotificationSender interface {
	TriggerMessage(userID, eventType string, msg sender.NotificationMessage)
}

// deptMemberResolver is the narrow slice of the dept-sync client the distribution
// service needs. It covers both halves of department-scoped distribution:
//   - recipient resolution: GetDeptUsersTree (a department's whole-subtree members);
//   - structural authorization: GetUserDepartments (which departments the operator
//     belongs to) + DepartmentSubtree (that department's node-with-children, so a
//     NON-LEAF membership confers management of the subtree) + GetDepartmentPath
//     (resolving a target's dept_path to range-check it) + Configured.
//
// Abstracted to a small interface so the department path can be unit-tested with a
// fake and so the service does not hard-depend on the concrete *deptsync.Client,
// which satisfies this interface.
type deptMemberResolver interface {
	GetDeptUsersTree(deptID string) ([]deptsync.DeptUser, error)
	GetDepartmentPath(deptID string) (string, error)
	GetUserDepartments(userID string) ([]deptsync.Dept, error)
	DepartmentSubtree(deptID string) (*deptsync.Dept, error)
	Configured() bool
}

// NewDistributionService creates a new distribution service. deptSync is optional;
// pass nil (or an unconfigured client) when department-scope distribution is not
// available — the department case then resolves to an empty recipient set rather
// than mis-firing.
func NewDistributionService(db *gorm.DB, behaviorSvc *BehaviorService, deptSync deptMemberResolver) *DistributionService {
	return &DistributionService{db: db, behaviorSvc: behaviorSvc, deptSync: deptSync}
}

// SetNotificationService sets the notification service for distribution events.
func (s *DistributionService) SetNotificationService(svc NotificationSender) {
	s.notificationSvc = svc
}

// DistributionTarget represents a single target in a distribute request.
type DistributionTarget struct {
	ScopeType string `json:"scopeType" binding:"required"` // user | organization | department
	TargetID  string `json:"targetId" binding:"required"`
}

// DistributeItemRequest represents a request to distribute an item.
type DistributeItemRequest struct {
	Targets        []DistributionTarget `json:"targets" binding:"required,min=1"`
	PermissionMode string               `json:"permissionMode" binding:"required,oneof=readonly dismissible"`
	Message        string               `json:"message"`
	ExpiresAt      *time.Time           `json:"expiresAt,omitempty"`
}

// DistributionResult holds the result of distributing to one target.
type DistributionResult struct {
	Distribution   *models.ItemDistribution `json:"distribution"`
	RecipientCount int                      `json:"recipientCount"`
}

var (
	ErrNotDistributor        = errors.New("only the distributor or platform admin can modify this distribution")
	ErrDistributionNotFound  = errors.New("distribution not found")
	ErrInvalidPermissionMode = errors.New("invalid permission mode")
	ErrCannotDistribute      = errors.New("you do not have permission to push this item")
	// ErrTargetOutOfScope is returned when a (non-platform-admin) department manager
	// aims a distribution at a department or user outside the subtree(s) they manage.
	ErrTargetOutOfScope = errors.New("target is outside your managed departments")
)

// CanDistribute reports whether operatorSubjectID may open the distribution flow for
// an item. Platform admins may always distribute (unrestricted). Otherwise the user
// may distribute iff they structurally manage at least one department subtree — i.e.
// they are the registered leader of some department (see resolveDistributionScope).
// This only gates *entry*; the per-target subtree check (authorizeTargets) is the
// real boundary on *where* they may distribute.
func (s *DistributionService) CanDistribute(item *models.CapabilityItem, operatorSubjectID string, isPlatformAdmin bool) bool {
	if isPlatformAdmin {
		return true
	}
	unlimited, prefixes, err := s.resolveDistributionScope(operatorSubjectID, false)
	if err != nil {
		return false
	}
	return unlimited || len(prefixes) > 0
}

// resolveDistributionScope computes a user's distribution reach:
//   - unlimited=true (platform admin): may distribute anywhere; prefixes is nil.
//   - otherwise: prefixes is the set of managed dept_path prefixes (the dept_paths of
//     every department the user leads). Each prefix covers itself and all descendants.
//
// It fails closed: when dept-sync is unconfigured/unreachable, the user cannot be
// resolved to a universal id, or the lookup errors, it returns (false, nil) — a
// non-admin then has no reach rather than an over-broad one. unlimited is decided
// purely by the platform-admin flag, never by dept-sync, so admin distribution is
// robust to dept-sync being down. Note: unlimited is intentionally NOT widened to
// business_admin / kanban "see all" — viewing scope must not bleed into push power.
func (s *DistributionService) resolveDistributionScope(operatorSubjectID string, isPlatformAdmin bool) (bool, []string, error) {
	if isPlatformAdmin {
		return true, nil, nil
	}
	led, err := s.ledDepartmentsFor(operatorSubjectID)
	if err != nil {
		// Degrade fail-closed: an unreachable dept-sync must not confer reach.
		return false, nil, nil
	}
	prefixes := make([]string, 0, len(led))
	seen := make(map[string]struct{}, len(led))
	for _, d := range led {
		if d.DeptPath == "" {
			continue
		}
		if _, ok := seen[d.DeptPath]; ok {
			continue
		}
		seen[d.DeptPath] = struct{}{}
		prefixes = append(prefixes, d.DeptPath)
	}
	return false, prefixes, nil
}

// DistributionAuthority is the operator's own distribution reach, surfaced to the
// frontend so it can show/hide the distribute entry and scope the department picker.
type DistributionAuthority struct {
	// Unlimited is true for platform admins (may distribute to anyone / any scope).
	Unlimited bool `json:"unlimited"`
	// Departments are the managed subtrees (the departments the user leads). For an
	// unlimited operator this is empty and the frontend uses the full admin tree.
	Departments []deptsync.Dept `json:"departments"`
}

// ResolveDistributionAuthority returns the operator's reach for the frontend entry
// gate. Platform admins are Unlimited; otherwise Departments lists the subtrees they
// lead (empty ⇒ no distribute entry). Fails soft: any dept-sync issue yields an
// empty, non-unlimited authority for a non-admin.
func (s *DistributionService) ResolveDistributionAuthority(operatorSubjectID string, isPlatformAdmin bool) (*DistributionAuthority, error) {
	if isPlatformAdmin {
		return &DistributionAuthority{Unlimited: true, Departments: []deptsync.Dept{}}, nil
	}
	if s.deptSync == nil || !s.deptSync.Configured() {
		return &DistributionAuthority{Unlimited: false, Departments: []deptsync.Dept{}}, nil
	}
	led, err := s.ledDepartmentsFor(operatorSubjectID)
	if err != nil || led == nil {
		led = []deptsync.Dept{}
	}
	return &DistributionAuthority{Unlimited: false, Departments: led}, nil
}

// ledDepartmentsFor returns the departments operatorSubjectID structurally manages —
// the single source of both their managed prefixes (authorization) and the authority
// subtree shown to the frontend. The rule is purely topological: a user who belongs
// to a NON-LEAF department (one with sub-departments) manages that department's whole
// subtree; members of leaf departments (rank-and-file) manage nothing. This needs no
// leader_id / position / 工号 — only the operator's department memberships and the
// tree shape.
//
// Returns (nil, nil) — i.e. no reach — when dept-sync is unavailable or the operator
// has no managing membership (fail closed). dept-sync read errors are surfaced so
// callers can distinguish "no reach" from "couldn't tell". The tree is read through
// the client's short-TTL cache (same cache the kanban-scope authz relies on), so a
// reorg takes effect on the next refresh — an accepted, bounded tradeoff consistent
// with the rest of the dept-sync-backed authz layer (and distributions are revocable).
func (s *DistributionService) ledDepartmentsFor(operatorSubjectID string) ([]deptsync.Dept, error) {
	if s.deptSync == nil || !s.deptSync.Configured() {
		return nil, nil
	}
	universalID := s.universalIDFor(operatorSubjectID)
	if universalID == "" {
		return nil, nil
	}
	myDepts, err := s.deptSync.GetUserDepartments(universalID)
	if err != nil {
		return nil, err
	}
	var managed []deptsync.Dept
	seen := make(map[string]struct{}, len(myDepts))
	for _, d := range myDepts {
		if d.DeptID == "" {
			continue
		}
		node, nerr := s.deptSync.DepartmentSubtree(d.DeptID)
		if nerr != nil {
			return nil, nerr
		}
		// Non-leaf membership ⇒ manager of that department's subtree. A leaf department
		// (no children) confers nothing.
		if node == nil || len(node.Children) == 0 {
			continue
		}
		if _, ok := seen[node.DeptPath]; ok {
			continue
		}
		seen[node.DeptPath] = struct{}{}
		managed = append(managed, *node)
	}
	return managed, nil
}

// authorizeTargets enforces the distribution boundary for non-platform-admins: every
// target must fall inside the operator's managed subtree(s). Platform admins pass
// unconditionally. A non-admin with no managed prefixes is rejected outright. Any one
// out-of-scope target rejects the whole request (atomic — no partial distribution).
//
//   - department target: its dept_path must be at-or-below a managed prefix.
//   - user target: the user must belong to at least one department at-or-below a
//     managed prefix (a member of the managed subtree).
//   - organization target: never allowed for a non-admin (company-level, cross-subtree).
func (s *DistributionService) AuthorizeTargets(operatorSubjectID string, isPlatformAdmin bool, targets []DistributionTarget) error {
	unlimited, prefixes, err := s.resolveDistributionScope(operatorSubjectID, isPlatformAdmin)
	if err != nil {
		return err
	}
	if unlimited {
		return nil
	}
	if len(prefixes) == 0 {
		return ErrCannotDistribute
	}
	for _, t := range targets {
		switch t.ScopeType {
		case "department":
			path, perr := s.deptSync.GetDepartmentPath(t.TargetID)
			if perr != nil || !anyPrefixMatch(path, prefixes) {
				return ErrTargetOutOfScope
			}
		case "user":
			ok, uerr := s.userWithinPrefixes(t.TargetID, prefixes)
			if uerr != nil || !ok {
				return ErrTargetOutOfScope
			}
		default:
			// organization / role / unknown: not delegable to a department manager.
			return ErrTargetOutOfScope
		}
	}
	return nil
}

// userWithinPrefixes reports whether the target user belongs to any department that
// is at-or-below one of the managed prefixes. The user is resolved to its universal
// id, then dept-sync gives their departments; a single membership inside the managed
// subtree is enough to be a reachable recipient.
func (s *DistributionService) userWithinPrefixes(targetSubjectID string, prefixes []string) (bool, error) {
	universalID := s.universalIDFor(targetSubjectID)
	if universalID == "" {
		return false, nil
	}
	depts, err := s.deptSync.GetUserDepartments(universalID)
	if err != nil {
		return false, err
	}
	for _, d := range depts {
		if anyPrefixMatch(d.DeptPath, prefixes) {
			return true, nil
		}
	}
	return false, nil
}

// universalIDFor maps a costrict-web subject_id to its casdoor_universal_id (the
// dept-sync bridge key). Returns "" when the user is unknown or unmapped — the same
// fail-closed bridge resolveRecipients uses for the department case.
func (s *DistributionService) universalIDFor(subjectID string) string {
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return ""
	}
	var user models.User
	if err := s.db.Select("casdoor_universal_id").
		Where("subject_id = ?", subjectID).First(&user).Error; err != nil {
		return ""
	}
	if user.CasdoorUniversalID == nil {
		return ""
	}
	return *user.CasdoorUniversalID
}

// anyPrefixMatch reports whether path is at-or-below any of the managed prefixes,
// using a '/' boundary so "/A/Bc" never matches a prefix "/A/B" (mirrors authz's
// pathHasPrefix). Empty path/prefix never matches.
func anyPrefixMatch(path string, prefixes []string) bool {
	if path == "" {
		return false
	}
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// DistributeItem distributes an item to the specified targets.
func (s *DistributionService) DistributeItem(ctx context.Context, item *models.CapabilityItem, distributorID string, req DistributeItemRequest) ([]DistributionResult, error) {
	results := make([]DistributionResult, 0, len(req.Targets))

	for _, target := range req.Targets {
		result, err := s.distributeToTarget(ctx, item, distributorID, target, req.PermissionMode, req.Message, req.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("distribute to target %s/%s failed: %w", target.ScopeType, target.TargetID, err)
		}
		results = append(results, *result)
	}

	return results, nil
}

func (s *DistributionService) distributeToTarget(ctx context.Context, item *models.CapabilityItem, distributorID string, target DistributionTarget, permissionMode, message string, expiresAt *time.Time) (*DistributionResult, error) {
	dist := &models.ItemDistribution{
		ID:             uuid.New().String(),
		ItemID:         item.ID,
		DistributorID:  distributorID,
		PermissionMode: permissionMode,
		Status:         "active",
		ScopeType:      target.ScopeType,
		TargetID:       target.TargetID,
		Message:        message,
		ExpiresAt:      expiresAt,
	}

	var recipientCount int
	var recipients []string

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(dist).Error; err != nil {
			return err
		}

		// Resolve recipients and create receipts
		resolved, err := s.resolveRecipients(tx, target)
		if err != nil {
			return err
		}
		recipients = resolved
		recipientCount = len(recipients)

		for _, userID := range recipients {
			receipt := models.ItemDistributionReceipt{
				ID:             uuid.New().String(),
				DistributionID: dist.ID,
				UserID:         userID,
				ReceiptStatus:  "unread",
			}
			// Use insert-ignore pattern to avoid duplicates
			if err := tx.Create(&receipt).Error; err != nil {
				// If duplicate, continue
				if !isUniqueConstraintError(err) {
					return err
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Auto-favorite each recipient AFTER the distribution has committed. This is
	// best-effort: a favorite hiccup must not roll back an otherwise-valid
	// distribution, and creating it post-commit avoids leaving an orphan favorite
	// if the transaction had rolled back. (In-tx it can't be best-effort: any error
	// inside the tx aborts the whole tx in Postgres.) The recipient is distributed
	// regardless; the favorite is the auto-install convenience.
	if s.behaviorSvc != nil {
		for _, userID := range recipients {
			_, _, _ = s.behaviorSvc.FavoriteItem(ctx, item.ID, userID)
		}
	}

	// Notify recipients. Carry the distributor's message (附言) into the body so
	// recipients actually see the note written at distribution time, not just the
	// generic "someone shared a skill" line.
	if s.notificationSvc != nil {
		body := fmt.Sprintf("有人向你下发了技能 **%s**（权限：%s）", item.Name, permissionMode)
		if strings.TrimSpace(message) != "" {
			body += fmt.Sprintf("\n\n附言：%s", message)
		}
		for _, userID := range recipients {
			s.notificationSvc.TriggerMessage(userID, "item.distributed", sender.NotificationMessage{
				Title:     "技能下发",
				Body:      body,
				EventType: "item.distributed",
				Metadata: map[string]any{
					"itemId":         item.ID,
					"itemName":       item.Name,
					"permissionMode": permissionMode,
					"distributionId": dist.ID,
					"message":        message,
				},
			})
		}
	}

	return &DistributionResult{
		Distribution:   dist,
		RecipientCount: recipientCount,
	}, nil
}

// resolveRecipients resolves the list of user IDs for a given target.
func (s *DistributionService) resolveRecipients(tx *gorm.DB, target DistributionTarget) ([]string, error) {
	switch target.ScopeType {
	case "user":
		return []string{target.TargetID}, nil
	case "organization":
		var userIDs []string
		// Exclude users without a subject_id (mirrors notification resolveBroadcastRecipients).
		if err := tx.Model(&models.User{}).Where("organization = ? AND subject_id <> ''", target.TargetID).Pluck("subject_id", &userIDs).Error; err != nil {
			return nil, err
		}
		return userIDs, nil
	case "department":
		// dept-sync is the source of truth for fine-grained departments. When it is
		// not injected/configured (or unreachable, surfaced as Configured()==false at
		// construction time) we must NOT fall back to "everyone" — resolve to an empty
		// recipient set so an unavailable dept-sync degrades to a no-op distribution
		// rather than mis-firing to all users.
		if s.deptSync == nil || !s.deptSync.Configured() {
			return []string{}, nil
		}
		members, err := s.deptSync.GetDeptUsersTree(target.TargetID)
		if err != nil {
			return nil, err
		}
		// Collect distinct, non-empty universal ids. A subtree may list the same
		// person under several sub-departments, so de-dup before the lookup.
		seen := make(map[string]struct{}, len(members))
		uids := make([]string, 0, len(members))
		for _, m := range members {
			if m.UniversalID == "" {
				continue
			}
			if _, ok := seen[m.UniversalID]; ok {
				continue
			}
			seen[m.UniversalID] = struct{}{}
			uids = append(uids, m.UniversalID)
		}
		if len(uids) == 0 {
			return []string{}, nil
		}
		// Bridge dept-sync universal_id -> local users.casdoor_universal_id ->
		// subject_id (the same bridge the admin org view uses). dept-sync members with
		// no matching local user (never signed into costrict-web) are silently skipped
		// by the IN filter — not an error.
		var subjectIDs []string
		if err := tx.Model(&models.User{}).
			Where("casdoor_universal_id IN ? AND subject_id <> ''", uids).
			Pluck("subject_id", &subjectIDs).Error; err != nil {
			return nil, err
		}
		// Pluck does not de-dup; guard against duplicate subject_ids (defensive — also
		// covers any future many-universal-id-to-one-subject mapping) so we never
		// create duplicate receipts.
		return dedupeStrings(subjectIDs), nil
	case "role":
		// Reserved for future extension
		return []string{}, nil
	default:
		return nil, fmt.Errorf("unsupported scope type: %s", target.ScopeType)
	}
}

// DistributionListFilter holds filters for the global (platform admin) distribution list.
type DistributionListFilter struct {
	Status    string // active | paused | revoked | "" (all)
	ScopeType string // user | organization | "" (all)
	Search    string // optional: matches item name / distributor id / target id
	Page      int    // 1-based
	PageSize  int    // defaults to 20
}

// ListAllDistributions lists distributions across all distributors (platform admin view),
// with optional status/scope/search filters and pagination.
func (s *DistributionService) ListAllDistributions(ctx context.Context, f DistributionListFilter) ([]models.ItemDistribution, int64, error) {
	q := s.db.WithContext(ctx).Model(&models.ItemDistribution{})

	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.ScopeType != "" {
		q = q.Where("scope_type = ?", f.ScopeType)
	}
	if f.Search != "" {
		like := "%" + f.Search + "%"
		q = q.Where(
			"distributor_id LIKE ? OR target_id LIKE ? OR item_id IN (?)",
			like, like,
			s.db.Model(&models.CapabilityItem{}).Select("id").Where("name LIKE ?", like),
		)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	page := f.Page
	if page < 1 {
		page = 1
	}
	size := f.PageSize
	if size <= 0 {
		size = 20
	}

	var list []models.ItemDistribution
	if err := q.
		Preload("Item").
		Order("created_at DESC").
		Offset((page - 1) * size).
		Limit(size).
		Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// ListReceipts lists all receipts for a given distribution (drives the detail drawer).
func (s *DistributionService) ListReceipts(ctx context.Context, distID string) ([]models.ItemDistributionReceipt, error) {
	var receipts []models.ItemDistributionReceipt
	if err := s.db.WithContext(ctx).
		Where("distribution_id = ?", distID).
		Order("created_at DESC").
		Find(&receipts).Error; err != nil {
		return nil, err
	}
	return receipts, nil
}

// ListItemDistributions lists all distributions for a given item.
func (s *DistributionService) ListItemDistributions(ctx context.Context, itemID string) ([]models.ItemDistribution, error) {
	var distributions []models.ItemDistribution
	if err := s.db.WithContext(ctx).Where("item_id = ?", itemID).Order("created_at DESC").Find(&distributions).Error; err != nil {
		return nil, err
	}
	return distributions, nil
}

// ListSentDistributions lists distributions sent by a user.
func (s *DistributionService) ListSentDistributions(ctx context.Context, distributorID string) ([]models.ItemDistribution, error) {
	var distributions []models.ItemDistribution
	if err := s.db.WithContext(ctx).Where("distributor_id = ?", distributorID).Preload("Item").Order("created_at DESC").Find(&distributions).Error; err != nil {
		return nil, err
	}
	return distributions, nil
}

// ListReceivedDistributions lists distributions received by a user (with item details).
func (s *DistributionService) ListReceivedDistributions(ctx context.Context, userID string) ([]models.ItemDistributionReceipt, error) {
	var receipts []models.ItemDistributionReceipt
	if err := s.db.WithContext(ctx).
		Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
		Where("item_distribution_receipts.user_id = ? AND item_distribution_receipts.receipt_status != ? AND item_distributions.status = ?", userID, "dismissed", "active").
		Preload("Distribution.Item").
		Order("item_distribution_receipts.created_at DESC").
		Find(&receipts).Error; err != nil {
		return nil, err
	}
	return receipts, nil
}

// GetDistributionByID fetches a distribution by ID.
func (s *DistributionService) GetDistributionByID(ctx context.Context, id string) (*models.ItemDistribution, error) {
	var dist models.ItemDistribution
	if err := s.db.WithContext(ctx).First(&dist, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrDistributionNotFound
		}
		return nil, err
	}
	return &dist, nil
}

// UpdateDistribution updates a distribution's status, permission mode, or message.
func (s *DistributionService) UpdateDistribution(ctx context.Context, distID, operatorID string, isPlatformAdmin bool, status, permissionMode, message *string) (*models.ItemDistribution, error) {
	dist, err := s.GetDistributionByID(ctx, distID)
	if err != nil {
		return nil, err
	}

	if !s.canModifyDistribution(dist, operatorID, isPlatformAdmin) {
		return nil, ErrNotDistributor
	}

	updates := make(map[string]interface{})
	if status != nil {
		updates["status"] = *status
		if *status == "revoked" {
			now := time.Now()
			updates["revoked_at"] = &now
		}
	}
	if permissionMode != nil {
		updates["permission_mode"] = *permissionMode
	}
	if message != nil {
		updates["message"] = *message
	}

	if len(updates) == 0 {
		return dist, nil
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(dist).Updates(updates).Error; err != nil {
			return err
		}

		// If revoked or paused, remove favorites for recipients
		if status != nil && (*status == "revoked" || *status == "paused") {
			var receipts []models.ItemDistributionReceipt
			if err := tx.Where("distribution_id = ?", dist.ID).Find(&receipts).Error; err != nil {
				return err
			}
			for _, receipt := range receipts {
				if s.behaviorSvc != nil {
					// Same tx as the status update, so the readonly guard sees this
					// distribution as already revoked/paused and removes the favorite
					// instead of treating the skill as still-required. ErrSkillRequired
					// (another active readonly distribution still needs it) is an
					// expected, non-fatal outcome — keep the favorite and continue. Any
					// OTHER error is a real failure: propagate it so the whole tx
					// (including the revoked/paused status change) rolls back rather than
					// committing a status that's out of sync with the favorite.
					if _, _, err := s.behaviorSvc.UnfavoriteItemTx(tx, dist.ItemID, receipt.UserID); err != nil && !errors.Is(err, ErrSkillRequired) {
						return err
					}
				}
			}
			if *status == "revoked" {
				if err := tx.Model(&models.ItemDistributionReceipt{}).Where("distribution_id = ?", dist.ID).Update("receipt_status", "dismissed").Error; err != nil {
					return err
				}
			}
		}

		// If resumed to active, re-add favorites
		if status != nil && *status == "active" {
			var receipts []models.ItemDistributionReceipt
			if err := tx.Where("distribution_id = ? AND receipt_status != ?", dist.ID, "dismissed").Find(&receipts).Error; err != nil {
				return err
			}
			for _, receipt := range receipts {
				if s.behaviorSvc != nil {
					// Re-favorite within the same tx; a real failure must roll back the
					// resume so status and favorite stay consistent.
					if _, _, err := s.behaviorSvc.FavoriteItemTx(tx, dist.ItemID, receipt.UserID); err != nil {
						return err
					}
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Notify recipients on revoke or pause
	if s.notificationSvc != nil && status != nil && (*status == "revoked" || *status == "paused") {
		var receipts []models.ItemDistributionReceipt
		if err := s.db.WithContext(ctx).Where("distribution_id = ?", dist.ID).Find(&receipts).Error; err == nil {
			var item models.CapabilityItem
			_ = s.db.WithContext(ctx).First(&item, "id = ?", dist.ItemID).Error
			for _, receipt := range receipts {
				body := fmt.Sprintf("技能 **%s** 的下发已被%s", item.Name, map[string]string{"revoked": "收回", "paused": "暂停"}[*status])
				s.notificationSvc.TriggerMessage(receipt.UserID, "item."+*status, sender.NotificationMessage{
					Title:     "技能下发更新",
					Body:      body,
					EventType: "item." + *status,
					Metadata: map[string]any{
						"itemId":         dist.ItemID,
						"itemName":       item.Name,
						"distributionId": dist.ID,
						"status":         *status,
					},
				})
			}
		}
	}

	return s.GetDistributionByID(ctx, distID)
}

// RevokeDistribution revokes a distribution (soft delete).
func (s *DistributionService) RevokeDistribution(ctx context.Context, distID, operatorID string, isPlatformAdmin bool) error {
	_, err := s.UpdateDistribution(ctx, distID, operatorID, isPlatformAdmin, strPtr("revoked"), nil, nil)
	return err
}

// DismissReceipt allows a recipient to dismiss a distribution from their view.
func (s *DistributionService) DismissReceipt(ctx context.Context, distID, userID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.ItemDistributionReceipt{}).
			Where("distribution_id = ? AND user_id = ?", distID, userID).
			Update("receipt_status", "dismissed")
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("receipt not found")
		}

		// Remove favorite
		var dist models.ItemDistribution
		if err := tx.Where("id = ?", distID).First(&dist).Error; err == nil {
			if s.behaviorSvc != nil {
				_, _, _ = s.behaviorSvc.UnfavoriteItem(ctx, dist.ItemID, userID)
			}
		}
		return nil
	})
}

// MarkReceiptRead marks a receipt as read.
func (s *DistributionService) MarkReceiptRead(ctx context.Context, distID, userID string) error {
	result := s.db.WithContext(ctx).Model(&models.ItemDistributionReceipt{}).
		Where("distribution_id = ? AND user_id = ?", distID, userID).
		Update("receipt_status", "read")
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("receipt not found")
	}
	return nil
}

// GetReceiptByDistributionAndUser gets a receipt for a specific distribution and user.
func (s *DistributionService) GetReceiptByDistributionAndUser(ctx context.Context, distID, userID string) (*models.ItemDistributionReceipt, error) {
	var receipt models.ItemDistributionReceipt
	if err := s.db.WithContext(ctx).Where("distribution_id = ? AND user_id = ?", distID, userID).First(&receipt).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("receipt not found")
		}
		return nil, err
	}
	return &receipt, nil
}

// GetEffectivePermission returns the effective permission mode (readonly |
// dismissible) for a user on an item, derived from the most recent active,
// non-dismissed distribution receipt. The bool reports whether such a
// distribution exists.
func (s *DistributionService) GetEffectivePermission(ctx context.Context, itemID, userID string) (string, bool) {
	var modes []string
	err := s.db.WithContext(ctx).
		Model(&models.ItemDistributionReceipt{}).
		Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
		Where("item_distribution_receipts.user_id = ? AND item_distributions.item_id = ? AND item_distributions.status = ? AND item_distribution_receipts.receipt_status != ?",
			userID, itemID, "active", "dismissed").
		Order("item_distributions.created_at DESC").
		Limit(1).
		Pluck("item_distributions.permission_mode", &modes).Error

	if err != nil || len(modes) == 0 {
		return "", false
	}
	return modes[0], true
}

// canModifyDistribution checks if an operator can modify a distribution.
func (s *DistributionService) canModifyDistribution(dist *models.ItemDistribution, operatorID string, isPlatformAdmin bool) bool {
	if isPlatformAdmin {
		return true
	}
	return dist.DistributorID == operatorID
}

// Helper functions

func strPtr(s string) *string {
	return &s
}

// dedupeStrings returns in with duplicates removed, preserving first-seen order.
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
