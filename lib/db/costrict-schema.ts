import {
  pgTable,
  uuid,
  varchar,
  text,
  integer,
  boolean,
  timestamp,
  decimal,
  index,
} from 'drizzle-orm/pg-core'

// Skill Repository table - stores repositories that contain skills/agents/commands
export const skillRepositories = pgTable(
  'skill_repositories',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    name: varchar('name', { length: 255 }).notNull(),
    description: text('description'),
    visibility: varchar('visibility', { length: 50 }).notNull().default('private'), // 'public' or 'private'
    ownerId: varchar('owner_id', { length: 191 }).notNull(),
    organizationId: varchar('organization_id', { length: 191 }),
    groupId: varchar('group_id', { length: 191 }),
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_skill_repo_owner').on(table.ownerId),
    index('idx_skill_repo_org').on(table.organizationId),
    index('idx_skill_repo_visibility').on(table.visibility),
  ]
)

// Skill table - stores skills
export const skills = pgTable(
  'skills',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    name: varchar('name', { length: 255 }).notNull(),
    description: text('description'),
    version: varchar('version', { length: 50 }),
    author: varchar('author', { length: 191 }),
    repoId: uuid('repo_id').notNull().references(() => skillRepositories.id, { onDelete: 'cascade' }),
    isPublic: boolean('is_public').notNull().default(false),
    installCount: integer('install_count').notNull().default(0),
    rating: decimal('rating', { precision: 3, scale: 2 }).notNull().default('0.00'),
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_skill_repo').on(table.repoId),
    index('idx_skill_public').on(table.isPublic),
    index('idx_skill_author').on(table.author),
  ]
)

// Agent table - stores agents
export const agents = pgTable(
  'agents',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    name: varchar('name', { length: 255 }).notNull(),
    description: text('description'),
    version: varchar('version', { length: 50 }),
    author: varchar('author', { length: 191 }),
    repoId: uuid('repo_id').notNull().references(() => skillRepositories.id, { onDelete: 'cascade' }),
    isPublic: boolean('is_public').notNull().default(false),
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_agent_repo').on(table.repoId),
  ]
)

// Command table - stores commands
export const commands = pgTable(
  'commands',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    name: varchar('name', { length: 255 }).notNull(),
    description: text('description'),
    repoId: uuid('repo_id').notNull().references(() => skillRepositories.id, { onDelete: 'cascade' }),
    author: varchar('author', { length: 191 }),
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_command_repo').on(table.repoId),
  ]
)

// MCP Server table - stores MCP servers
export const mcpServers = pgTable(
  'mcp_servers',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    name: varchar('name', { length: 255 }).notNull(),
    description: text('description'),
    repoId: uuid('repo_id').notNull().references(() => skillRepositories.id, { onDelete: 'cascade' }),
    author: varchar('author', { length: 191 }),
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_mcp_server_repo').on(table.repoId),
  ]
)

// Skill Rating table - stores skill ratings
export const skillRatings = pgTable(
  'skill_ratings',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    skillId: uuid('skill_id').notNull().references(() => skills.id, { onDelete: 'cascade' }),
    userId: varchar('user_id', { length: 191 }).notNull(),
    rating: integer('rating').notNull(), // 1-5
    comment: text('comment'),
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_rating_skill').on(table.skillId),
    index('idx_rating_user').on(table.userId),
  ]
)

// User Preference table - stores user preferences
export const userPreferences = pgTable(
  'user_preferences',
  {
    id: uuid('id').primaryKey().defaultRandom(),
    userId: varchar('user_id', { length: 191 }).notNull().unique(),
    defaultRepositoryId: uuid('default_repository_id').references(() => skillRepositories.id, { onDelete: 'set null' }),
    favoriteSkills: text('favorite_skills'), // JSON array
    skillPermissions: text('skill_permissions'), // JSON object
    createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
    updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
  },
  (table) => [
    index('idx_user_pref_user').on(table.userId),
  ]
)
