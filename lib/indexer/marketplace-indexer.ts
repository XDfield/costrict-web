import { db } from '@/lib/db/client'
import { marketplaces, marketplaceStats } from '@/lib/db/schema'
import { getGitHubClient, GitHubRepo } from '@/lib/github/client'
import { parseMarketplaceJson, extractCounts, extractCategories as extractPluginCategories } from './parser'
import { eq, sql } from 'drizzle-orm'

// Known/seed marketplaces with accurate fallback counts
// Fallback counts are used when marketplace.json parsing fails
const KNOWN_MARKETPLACES = [
  {
    repo: 'davepoon/buildwithclaude',
    url: 'https://buildwithclaude.com',
    displayName: 'Build with Claude',
    description: 'Curated collection of Claude Code plugins, skills, subagents, commands, and hooks.',
    categories: ['subagents', 'commands', 'hooks', 'skills'],
    badges: ['featured'],
    fallbackPluginCount: 50, // 50 plugins in marketplace.json
    fallbackSkillCount: 26, // 26 skills across plugins
  },
  {
    repo: 'Kamalnrf/claude-plugins',
    url: 'https://claude-plugins.dev',
    displayName: 'Claude Plugins',
    description: 'Community registry with CLI for discovering and installing Claude Code plugins.',
    categories: ['plugins', 'skills'],
    badges: ['popular', 'featured'],
    fallbackPluginCount: 100,
    fallbackSkillCount: 50,
  },
  {
    repo: 'ananddtyagi/claude-code-marketplace',
    url: 'https://claudecodecommands.directory',
    displayName: 'Claude Code Marketplace',
    description: 'Community-driven marketplace for Claude Code commands and plugins.',
    categories: ['commands', 'agents'],
    badges: [],
    fallbackPluginCount: 50,
    fallbackSkillCount: 20,
  },
  {
    repo: 'jeremylongshore/claude-code-plugins-plus-skills',
    url: 'https://github.com/jeremylongshore/claude-code-plugins-plus-skills',
    displayName: 'Plugins Plus Skills',
    description: 'Production-ready Agent Skills with interactive Jupyter tutorials.',
    categories: ['skills', 'tutorials'],
    badges: [],
    fallbackPluginCount: 10,
    fallbackSkillCount: 15,
  },
  {
    repo: 'anthropics/claude-code-plugins',
    url: 'https://github.com/anthropics/claude-code-plugins',
    displayName: 'Official Claude Plugins',
    description: 'Official Claude Code plugins from Anthropic.',
    categories: ['official', 'plugins'],
    badges: ['official'],
    fallbackPluginCount: 5,
    fallbackSkillCount: 5,
  },
]

export interface IndexResult {
  indexed: number
  failed: number
  durationMs: number
}

/**
 * Main indexing function - discovers and indexes plugin marketplaces
 */
export async function indexMarketplaces(): Promise<IndexResult> {
  const startTime = Date.now()
  const github = getGitHubClient()

  let indexed = 0
  let failed = 0

  // Collect repos to process
  const repoSet = new Set<string>()

  // Add known marketplaces
  KNOWN_MARKETPLACES.forEach((m) => repoSet.add(m.repo))

  // Search GitHub for marketplace.json files
  const searchQuery = 'filename:marketplace.json path:.claude-plugin'

  try {
    const searchResult = await github.searchCode(searchQuery)
    console.log(`Found ${searchResult.total_count} marketplace.json files on GitHub`)

    // Process up to 10 pages (1000 results max due to GitHub API limit)
    const maxPages = Math.min(Math.ceil(searchResult.total_count / 100), 10)

    for (let page = 1; page <= maxPages; page++) {
      const pageResult = page === 1 ? searchResult : await github.searchCode(searchQuery, page)

      for (const item of pageResult.items) {
        repoSet.add(item.repository.full_name)
      }
    }
  } catch (error) {
    console.error('GitHub search failed:', error)
    // Continue with known marketplaces
  }

  // Also search for claude-code-marketplace topic
  try {
    // Note: Topic search would require different API endpoint
    // For now we rely on marketplace.json discovery
  } catch (error) {
    console.error('Topic search failed:', error)
  }

  console.log(`Processing ${repoSet.size} repositories...`)

  // Process each repo
  for (const repoFullName of repoSet) {
    try {
      const result = await processRepository(repoFullName)
      if (result) {
        indexed++
      }
    } catch (error) {
      console.error(`Failed to process ${repoFullName}:`, error)
      failed++
    }
  }

  return {
    indexed,
    failed,
    durationMs: Date.now() - startTime,
  }
}

/**
 * Process a single repository to determine if it's a marketplace
 */
async function processRepository(repoFullName: string): Promise<boolean> {
  const github = getGitHubClient()

  // Fetch repo metadata
  const repoMeta = await github.fetchRepoMetadata(repoFullName)

  // Skip forks
  if (repoMeta.fork) {
    console.log(`Skipping ${repoFullName}: is a fork`)
    return false
  }

  // Check if this is a marketplace (not just a single plugin)
  if (!isMarketplace(repoMeta, repoFullName)) {
    console.log(`Skipping ${repoFullName}: not a marketplace`)
    return false
  }

  // Try to fetch marketplace.json
  let pluginCount = 0
  let skillCount = 0
  let jsonCategories: string[] = []

  try {
    const content = await github.fetchFileContent(repoFullName, '.claude-plugin/marketplace.json')
    const marketplace = parseMarketplaceJson(content, repoFullName)

    if (marketplace) {
      const counts = extractCounts(marketplace)
      pluginCount = counts.pluginCount
      skillCount = counts.skillCount
      jsonCategories = extractPluginCategories(marketplace)
      console.log(`  Parsed ${repoFullName}: ${pluginCount} plugins, ${skillCount} skills`)
    } else {
      console.log(`  Failed to parse marketplace.json from ${repoFullName}`)
    }
  } catch (error) {
    // Try alternate path
    try {
      const content = await github.fetchFileContent(repoFullName, 'marketplace.json')
      const marketplace = parseMarketplaceJson(content, repoFullName)

      if (marketplace) {
        const counts = extractCounts(marketplace)
        pluginCount = counts.pluginCount
        skillCount = counts.skillCount
        jsonCategories = extractPluginCategories(marketplace)
        console.log(`  Parsed ${repoFullName} (alt path): ${pluginCount} plugins, ${skillCount} skills`)
      } else {
        console.log(`  Failed to parse marketplace.json from ${repoFullName} (alt path)`)
      }
    } catch {
      // No marketplace.json found, use estimated counts from known marketplaces
      console.log(`  No marketplace.json found in ${repoFullName}, using fallback counts`)
    }
  }

  // Check for known marketplace overrides
  const known = KNOWN_MARKETPLACES.find((m) => m.repo === repoFullName)

  const namespace = `@${repoMeta.owner.login}/${repoMeta.name}`

  // Upsert marketplace
  await db
    .insert(marketplaces)
    .values({
      name: repoMeta.name,
      displayName: known?.displayName || formatDisplayName(repoMeta.name),
      namespace,
      url: known?.url || repoMeta.homepage || repoMeta.html_url,
      repository: repoMeta.html_url,
      installCommand: `/plugin marketplace add ${repoFullName}`,
      pluginCount: pluginCount || known?.fallbackPluginCount || 0,
      skillCount: skillCount || known?.fallbackSkillCount || 0,
      description: known?.description || repoMeta.description || '',
      categories: known?.categories || jsonCategories.length > 0 ? jsonCategories : extractRepoCategories(repoMeta),
      badges: known?.badges || [],
      maintainerName: repoMeta.owner.login,
      maintainerGithub: repoMeta.owner.login,
      stars: repoMeta.stargazers_count,
      verified: known?.badges?.includes('official') || false,
      lastIndexedAt: new Date(),
    })
    .onConflictDoUpdate({
      target: marketplaces.namespace,
      set: {
        displayName: sql`COALESCE(EXCLUDED.display_name, ${marketplaces.displayName})`,
        url: sql`EXCLUDED.url`,
        pluginCount: sql`EXCLUDED.plugin_count`,
        skillCount: sql`EXCLUDED.skill_count`,
        description: sql`COALESCE(EXCLUDED.description, ${marketplaces.description})`,
        categories: sql`EXCLUDED.categories`,
        stars: sql`EXCLUDED.stars`,
        lastIndexedAt: sql`EXCLUDED.last_indexed_at`,
        updatedAt: sql`NOW()`,
      },
    })

  // Record stats snapshot
  const existingMarketplace = await db
    .select({ id: marketplaces.id })
    .from(marketplaces)
    .where(eq(marketplaces.namespace, namespace))
    .limit(1)

  if (existingMarketplace.length > 0) {
    await db.insert(marketplaceStats).values({
      marketplaceId: existingMarketplace[0].id,
      pluginCount,
      skillCount,
      stars: repoMeta.stargazers_count,
    })
  }

  console.log(`Indexed marketplace: ${repoFullName}`)
  return true
}

/**
 * Determine if a repository is a marketplace (collection of plugins)
 */
function isMarketplace(repo: GitHubRepo, repoFullName: string): boolean {
  // Known repos are always marketplaces
  if (KNOWN_MARKETPLACES.some((m) => m.repo === repoFullName)) {
    return true
  }

  // Check topics
  const marketplaceTopics = ['marketplace', 'registry', 'collection', 'awesome', 'claude-code-plugins']
  if (repo.topics?.some((t) => marketplaceTopics.includes(t.toLowerCase()))) {
    return true
  }

  // Check name/description for marketplace indicators
  const indicators = ['marketplace', 'registry', 'collection', 'plugins', 'awesome']
  const nameDesc = `${repo.name} ${repo.description || ''}`.toLowerCase()
  if (indicators.some((ind) => nameDesc.includes(ind))) {
    return true
  }

  // If it has a marketplace.json, it's likely a marketplace
  // This is the default case since we found it via search
  return true
}

/**
 * Format repository name as display name
 */
function formatDisplayName(name: string): string {
  return name
    .split(/[-_]/)
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ')
}

/**
 * Extract categories from repository topics
 */
function extractRepoCategories(repo: GitHubRepo): string[] {
  const categories = new Set<string>()

  for (const topic of repo.topics || []) {
    // Skip generic topics
    if (!topic.includes('claude') && !topic.includes('plugin') && !topic.includes('mcp')) {
      categories.add(topic)
    }
  }

  return Array.from(categories).slice(0, 5)
}
