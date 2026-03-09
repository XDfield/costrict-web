import {
  pgTable,
  uuid,
  varchar,
  text,
  integer,
  boolean,
  timestamp,
  json,
  jsonb,
  index,
  pgEnum,
} from 'drizzle-orm/pg-core'

// User roles enum
export const userRoleEnum = pgEnum('user_role', ['admin', 'editor', 'viewer'])

// Resource types enum
export const resourceTypeEnum = pgEnum('resource_type', ['mcp_server', 'agent', 'command', 'hook', 'skill', 'plugin'])

// Resource visibility enum
export const visibilityEnum = pgEnum('visibility', ['private', 'organization', 'public'])

// Organizations table - enterprise organizations
export const organizations = pgTable(
  'organizations',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    
    // Basic info
    name: varchar('name', { length: 255 }).notNull(),
    slug: varchar('slug', { length: 255 }).notNull().unique(),
    description: text('description'),
    logoUrl: varchar('logo_url', { length: 512 }),
    
    // Contact info
    contactEmail: varchar('contact_email', { length: 255 }),
    website: varchar('website', { length: 512 }),
    
    // Subscription/Billing
    plan: varchar('plan', { length: 64 }).default('free'), // free, pro, enterprise
    maxUsers: integer('max_users').default(5),
    maxResources: integer('max_resources').default(100),
    
    // Settings
    settings: jsonb('settings').$type<{
      allowPublicResources: boolean
      requireApproval: boolean
      customDomains: string[]
      ssoEnabled: boolean
      apiRateLimit: number
    }>(),
    
    // Status
    active: boolean('active').notNull().default(true),
    verified: boolean('verified').notNull().default(false),
    
    // Timestamps
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_organizations_slug').on(table.slug),
    index('idx_organizations_active').on(table.active),
    index('idx_organizations_plan').on(table.plan),
  ]
)

// Users table - platform users
export const users = pgTable(
  'users',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    
    // Auth info
    email: varchar('email', { length: 255 }).notNull().unique(),
    passwordHash: varchar('password_hash', { length: 255 }),
    name: varchar('name', { length: 255 }),
    avatarUrl: varchar('avatar_url', { length: 512 }),
    
    // OAuth providers
    provider: varchar('provider', { length: 64 }), // github, google, saml
    providerId: varchar('provider_id', { length: 255 }),
    
    // Status
    active: boolean('active').notNull().default(true),
    emailVerified: boolean('email_verified').notNull().default(false),
    
    // Timestamps
    lastLoginAt: timestamp('last_login_at', { withTimezone: true }),
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_users_email').on(table.email),
    index('idx_users_provider').on(table.provider, table.providerId),
    index('idx_users_active').on(table.active),
  ]
)

// Organization members table - user-organization relationship
export const organizationMembers = pgTable(
  'organization_members',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    
    // Relations
    organizationId: uuid('organization_id')
      .notNull()
      .references(() => organizations.id, { onDelete: 'cascade' }),
    userId: uuid('user_id')
      .notNull()
      .references(() => users.id, { onDelete: 'cascade' }),
    
    // Role and permissions
    role: userRoleEnum('role').notNull().default('viewer'),
    permissions: text('permissions').array(), // granular permissions
    
    // Status
    active: boolean('active').notNull().default(true),
    invitedBy: uuid('invited_by').references(() => users.id),
    
    // Timestamps
    joinedAt: timestamp('joined_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_org_members_org').on(table.organizationId),
    index('idx_org_members_user').on(table.userId),
    index('idx_org_members_role').on(table.role),
  ]
)

// Custom resources table - stores user/organization resources
export const customResources = pgTable(
  'custom_resources',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    
    // Ownership
    organizationId: uuid('organization_id')
      .notNull()
      .references(() => organizations.id, { onDelete: 'cascade' }),
    createdBy: uuid('created_by')
      .notNull()
      .references(() => users.id),
    updatedBy: uuid('updated_by').references(() => users.id),
    
    // Resource identification
    name: varchar('name', { length: 255 }).notNull(),
    slug: varchar('slug', { length: 255 }).notNull(),
    type: resourceTypeEnum('type').notNull(),
    category: varchar('category', { length: 128 }),
    
    // Content (stored as JSON for flexibility)
    content: jsonb('content').notNull(), // Resource-specific data
    config: jsonb('config'), // Configuration metadata
    
    // Metadata
    description: text('description'),
    tags: text('tags').array(),
    version: varchar('version', { length: 64 }).default('1.0.0'),
    
    // Visibility and access control
    visibility: visibilityEnum('visibility').notNull().default('private'),
    allowedUsers: uuid('allowed_users').array().references(() => users.id),
    allowedTeams: text('allowed_teams').array(),
    
    // Deployment settings
    deploymentConfig: jsonb('deployment_config').$type<{
      autoDeploy: boolean
      deploymentEnvironments: string[]
      webhookUrl: string
      healthCheckEnabled: boolean
    }>(),
    
    // Status
    status: varchar('status', { length: 64 }).default('draft'), // draft, published, archived, deprecated
    active: boolean('active').notNull().default(true),
    featured: boolean('featured').notNull().default(false),
    
    // Usage tracking
    installCount: integer('install_count').default(0),
    viewCount: integer('view_count').default(0),
    lastInstalledAt: timestamp('last_installed_at', { withTimezone: true }),
    
    // Timestamps
    publishedAt: timestamp('published_at', { withTimezone: true }),
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_custom_resources_org').on(table.organizationId),
    index('idx_custom_resources_type').on(table.type),
    index('idx_custom_resources_slug').on(table.slug),
    index('idx_custom_resources_visibility').on(table.visibility),
    index('idx_custom_resources_status').on(table.status),
    index('idx_custom_resources_category').on(table.category),
    index('idx_custom_resources_created_by').on(table.createdBy),
  ]
)

// Resource versions table - version history
export const resourceVersions = pgTable(
  'resource_versions',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    
    // Reference to resource
    resourceId: uuid('resource_id')
      .notNull()
      .references(() => customResources.id, { onDelete: 'cascade' }),
    
    // Version info
    version: varchar('version', { length: 64 }).notNull(),
    versionNumber: integer('version_number').notNull(), // 1, 2, 3...
    
    // Content snapshot
    content: jsonb('content').notNull(),
    config: jsonb('config'),
    
    // Change metadata
    changeLog: text('change_log'),
    changedBy: uuid('changed_by')
      .notNull()
      .references(() => users.id),
    changeType: varchar('change_type', { length: 64 }), // created, updated, rolled_back
    
    // Timestamps
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_resource_versions_resource').on(table.resourceId),
    index('idx_resource_versions_version').on(table.version),
    index('idx_resource_versions_created').on(table.createdAt),
  ]
)

// Resource deployments table - track deployments
export const resourceDeployments = pgTable(
  'resource_deployments',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    
    // Reference to resource
    resourceId: uuid('resource_id')
      .notNull()
      .references(() => customResources.id, { onDelete: 'cascade' }),
    versionId: uuid('version_id').references(() => resourceVersions.id),
    
    // Deployment info
    environment: varchar('environment', { length: 64 }).notNull(), // development, staging, production
    deploymentUrl: varchar('deployment_url', { length: 512 }),
    status: varchar('status', { length: 64 }).default('pending'), // pending, success, failed, rolling_back
    
    // Metadata
    deployedBy: uuid('deployed_by')
      .notNull()
      .references(() => users.id),
    deploymentLog: text('deployment_log'),
    rollbackVersionId: uuid('rollback_version_id').references(() => resourceVersions.id),
    
    // Timestamps
    startedAt: timestamp('started_at', { withTimezone: true }).notNull().defaultNow(),
    completedAt: timestamp('completed_at', { withTimezone: true }),
  },
  (table) => [
    index('idx_resource_deployments_resource').on(table.resourceId),
    index('idx_resource_deployments_environment').on(table.environment),
    index('idx_resource_deployments_status').on(table.status),
    index('idx_resource_deployments_started').on(table.startedAt),
  ]
)

// Resource analytics table - usage analytics
export const resourceAnalytics = pgTable(
  'resource_analytics',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    
    // Reference
    resourceId: uuid('resource_id')
      .notNull()
      .references(() => customResources.id, { onDelete: 'cascade' }),
    
    // Metrics
    metricType: varchar('metric_type', { length: 64 }).notNull(), // install, view, error, usage
    metricValue: integer('metric_value').notNull().default(1),
    
    // Context
    context: jsonb('context'), // Additional context (user_id, ip, etc.)
    
    // Timestamp
    recordedAt: timestamp('recorded_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_resource_analytics_resource').on(table.resourceId),
    index('idx_resource_analytics_type').on(table.metricType),
    index('idx_resource_analytics_recorded').on(table.recordedAt),
  ]
)

// API keys table - for programmatic access
export const apiKeys = pgTable(
  'api_keys',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    
    // Ownership
    organizationId: uuid('organization_id')
      .notNull()
      .references(() => organizations.id, { onDelete: 'cascade' }),
    createdBy: uuid('created_by')
      .notNull()
      .references(() => users.id),
    
    // Key info
    name: varchar('name', { length: 255 }).notNull(),
    keyHash: varchar('key_hash', { length: 255 }).notNull(),
    keyPrefix: varchar('key_prefix', { length: 16 }).notNull(), // For display (e.g., "bwc_...")
    
    // Permissions and scope
    scopes: text('scopes').array(), // resources:read, resources:write, deployments:manage
    rateLimit: integer('rate_limit').default(1000), // requests per hour
    allowedIPs: text('allowed_ips').array(),
    
    // Status
    active: boolean('active').notNull().default(true),
    expiresAt: timestamp('expires_at', { withTimezone: true }),
    lastUsedAt: timestamp('last_used_at', { withTimezone: true }),
    
    // Timestamps
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_api_keys_org').on(table.organizationId),
    index('idx_api_keys_key_hash').on(table.keyHash),
    index('idx_api_keys_active').on(table.active),
  ]
)

// Webhooks table - for event notifications
export const webhooks = pgTable(
  'webhooks',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    
    // Ownership
    organizationId: uuid('organization_id')
      .notNull()
      .references(() => organizations.id, { onDelete: 'cascade' }),
    createdBy: uuid('created_by')
      .notNull()
      .references(() => users.id),
    
    // Webhook info
    name: varchar('name', { length: 255 }).notNull(),
    url: varchar('url', { length: 512 }).notNull(),
    secret: varchar('secret', { length: 255 }),
    
    // Events
    events: text('events').array().notNull(), // resource.created, resource.updated, deployment.success, etc.
    
    // Configuration
    active: boolean('active').notNull().default(true),
    retryConfig: jsonb('retry_config').$type<{
      maxRetries: number
      retryDelay: number
    }>(),
    
    // Stats
    deliveryCount: integer('delivery_count').default(0),
    failureCount: integer('failure_count').default(0),
    lastTriggeredAt: timestamp('last_triggered_at', { withTimezone: true }),
    
    // Timestamps
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_webhooks_org').on(table.organizationId),
    index('idx_webhooks_active').on(table.active),
  ]
)

// Type exports
export type Organization = typeof organizations.$inferSelect
export type NewOrganization = typeof organizations.$inferInsert
export type User = typeof users.$inferSelect
export type NewUser = typeof users.$inferInsert
export type OrganizationMember = typeof organizationMembers.$inferSelect
export type NewOrganizationMember = typeof organizationMembers.$inferInsert
export type CustomResource = typeof customResources.$inferSelect
export type NewCustomResource = typeof customResources.$inferInsert
export type ResourceVersion = typeof resourceVersions.$inferSelect
export type NewResourceVersion = typeof resourceVersions.$inferInsert
export type ResourceDeployment = typeof resourceDeployments.$inferSelect
export type NewResourceDeployment = typeof resourceDeployments.$inferInsert
export type ResourceAnalytics = typeof resourceAnalytics.$inferSelect
export type NewResourceAnalytics = typeof resourceAnalytics.$inferInsert
export type ApiKey = typeof apiKeys.$inferSelect
export type NewApiKey = typeof apiKeys.$inferInsert
export type Webhook = typeof webhooks.$inferSelect
export type NewWebhook = typeof webhooks.$inferInsert