# ITEM_MULTI_TAG Progress

## Background

This task tracks the implementation of the revised multi-tag design for capability items.

Related proposal:

- `docs/proposals/ITEM_MULTI_TAG_DESIGN.md`

## Goals

- Support three tag types: `system`, `builtin`, and `custom`
- Remove i18n name/description/update-time fields from tags
- Enforce strict slug validation: `^[a-z0-9_-]+$`
- Add tag list query with keyword search and pagination
- Restrict tag create/update/delete to system administrators only
- Prevent non-admin users from assigning `system` tags through APIs
- Initialize system tags in migration
- Keep item list tag filtering capability

---

## Scope

### In Scope

- Tag model changes
- Migration updates
- Tag service updates
- Tag API changes
- Item tag assignment restrictions
- Sync/create flow adjustments
- Route registration
- Tests and docs updates

### Out of Scope

- Frontend UI changes
- Semantic search tag filtering
- Non-tag category redesign

---

## Task Breakdown

## 1. Proposal Sync

- [x] Update `docs/proposals/ITEM_MULTI_TAG_DESIGN.md`
- [x] Reflect new tag type model: `system` / `builtin` / `custom`
- [x] Reflect new admin-only permissions
- [x] Reflect slug validation and list query requirements

## 2. Data Model

- [x] Update `server/internal/models/models.go`
- [x] Remove `names` from `ItemTagDict`
- [x] Remove `descriptions` from `ItemTagDict`
- [x] Remove `updated_at` / `UpdatedAt` from `ItemTagDict`
- [x] Keep `CapabilityItem.Tags` virtual field

## 3. Migration

- [x] Update `server/migrations/20260422100000_create_item_tags.sql`
- [x] Simplify `item_tag_dicts` schema
- [x] Keep `item_tags` relation table
- [x] Seed system and builtin tags in migration
- [x] Backfill existing active items with system tags by `item_type`
- [x] Remove old `functional` tag seed/backfill logic

## 4. Tag Service

- [x] Remove `TagClassFunctional`
- [x] Add slug validator helper
- [x] Normalize slug input with trim + lowercase
- [x] Enforce regex `^[a-z0-9_-]+$`
- [x] Update `EnsureTags` to support `system/builtin/custom`
- [x] Update duplicate-key handling to include `duplicated key not allowed`
- [x] Change list API support to query + pagination

## 5. Tag Handlers

- [x] Update `server/internal/handlers/tag.go`
- [x] Add `q` query support for slug fuzzy match
- [x] Add `page` and `pageSize`
- [x] Return `total/page/pageSize/hasMore`
- [x] Restrict create tag to system admin only
- [x] Restrict update tag to system admin only
- [x] Restrict delete tag to system admin only
- [x] Restrict assigning `system` tags to system admin only while allowing `builtin` tags for normal users

## 6. Route Registration

- [x] Update `server/cmd/api/main.go`
- [x] Register `POST /api/tags`
- [x] Register `PUT /api/tags/:id`
- [x] Register `DELETE /api/tags/:id`
- [x] Register `POST /api/items/:id/tags`
- [x] Apply system admin authorization to tag write routes

## 7. Item Create / Update Flow

- [x] Update `server/internal/handlers/capability_item.go`
- [x] JSON item creation should auto-create only `custom` tags from request while allowing existing `builtin` tags
- [x] Reject non-admin assignment of existing `system` tags via request tags
- [x] Archive-based creation should treat parsed tags as `custom`
- [x] Reject non-admin assignment when parsed tags resolve to `system`
- [x] Remove system auto-tagging by `item_type`

## 8. Sync Flow

- [x] Update `server/internal/services/sync_service.go`
- [x] Replace old `functional` usage with `custom`
- [x] Keep parsed tags from source files as `custom`
- [x] Remove system auto-tagging during sync

## 9. Tag Query / Response Coverage

- [x] Verify `GET /api/items` keeps tag filtering support
- [x] Verify `GET /api/items/my` keeps tag filtering support
- [x] Decide whether `GET /api/items/:id` should include tags
- [x] Decide whether create item response should include tags

## 10. Tests

- [x] Update model/service tests for simplified tag schema
- [x] Add slug validation tests
- [x] Add tag list search tests
- [x] Add tag list pagination tests
- [x] Add admin-only create/update/delete tag tests
- [x] Add builtin-tag assignment tests for normal users
- [x] Add non-admin system-tag assignment rejection tests
- [x] Add admin system-tag assignment success tests
- [x] Add sync/create flow tests for `custom` tag behavior
- [ ] Add migration/system-tag seed verification if applicable

## 11. Swagger / Docs

- [x] Update swagger annotations in handlers
- [x] Update tag response schema references
- [x] Ensure docs match three-tag model

---

## Open Decisions

### D1. System auto-tagging

Need final confirmation:

- Should the backend always auto-attach the matching `system` tag based on `item_type` for newly created/synced items?

Decision: **No**. `item_type` no longer implies automatic `system` tag attachment during create or sync.

### D1.1 Tag class model

- Should the system use two classes or three classes?

Decision: **Three classes**: `system`, `builtin`, `custom`.

### D1.2 Builtin tag assignment

- Should normal users be allowed to assign builtin tags to items?

Decision: **Yes**. `builtin` tags are system-built-in standard tags and may be selected by users.

### D2. Behavior when parsed/request tags collide with existing `system` tags

Need final confirmation:

- For non-admin users, should the request fail immediately if any supplied tag slug resolves to a `system` tag?

Decision: **No**. Silently filter out user-supplied `system` tags at the data layer.

### D3. Tag response coverage

Need final confirmation:

- Should `GET /api/items/:id` and create-item responses include populated `tags`?

Decision: **Yes**. Both `GET /api/items/:id` and create-item responses now include populated `tags`.

---

## Progress Log

### 2026-04-23

- Revised proposal synced to new tag model and permission model
- Implementation checklist created
- Core backend implementation completed for model, migration, service, handlers, routes, and focused tests
- Non-admin user supplied system tags are silently filtered instead of rejected
- Item detail and create responses now include populated tags
- Admin-only tag create/update/delete route tests completed
- Removed automatic `item_type` -> `system` tag attachment from create and sync flows
- Proposal updated to three tag classes: `system`, `builtin`, `custom`
- Migration and backend logic updated for `official` / `best-practice` system tags and builtin stage tags
