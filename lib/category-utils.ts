/**
 * Utility functions for dynamic category management
 */

// Special case mappings for better display names
const SPECIAL_CASES: Record<string, string> = {
  'ai': 'AI',
  'api': 'API',
  'ui': 'UI',
  'ux': 'UX',
  'defi': 'DeFi',
  'ml': 'ML',
  'ci': 'CI',
  'cd': 'CD'
};

// Icon mappings for categories
export const CATEGORY_ICONS: Record<string, string> = {
  // Plugin categories
  'utilities': '🔧',
  'agents': '🤖',
  'commands': '⌨️',
  'hooks': '🪝',
  'mcp-servers': '🔌',
  // Subagent categories
  'development-architecture': '🏗️',
  'language-specialists': '💻',
  'infrastructure-operations': '🚀',
  'quality-security': '🛡️',
  'data-ai': '📊',
  'specialized-domains': '🎯',
  'crypto-trading': '💰',
  'business-finance': '💼',
  'design-experience': '🎨',
  'blockchain-web3': '🔗',
  'sales-marketing': '📣',
  // Command categories
  'api-development': '🔌',
  'automation-workflow': '⚙️',
  'ci-deployment': '🔄',
  'code-analysis-testing': '🧪',
  'context-loading-priming': '📥',
  'database-operations': '🗄️',
  'documentation-changelogs': '📝',
  'framework-svelte': '🔥',
  'game-development': '🎮',
  'integration-sync': '🔗',
  'miscellaneous': '🔧',
  'monitoring-observability': '📊',
  'performance-optimization': '⚡',
  'project-setup': '🏁',
  'project-task-management': '📋',
  'security-audit': '🔒',
  'simulation-modeling': '🔮',
  'team-collaboration': '👥',
  'typescript-migration': '📘',
  'utilities-debugging': '🐛',
  'version-control-git': '🌿',
  'workflow-orchestration': '🎭',
  // Hook categories
  'git': '🌿',
  'automation': '⚙️',
  'notifications': '🔔',
  'formatting': '✨',
  'security': '🔒',
  'testing': '🧪',
  'development': '💻',
  'performance': '⚡',
  // Skill categories
  'document-processing': '📄',
  'development-code': '💻',
  'business-productivity': '📊',
  'creative-collaboration': '🎨',
  'analytics': '📈',
  'communication': '💬',
  'crm': '🤝',
  'design': '🎨',
  'devops': '🚀',
  'ecommerce': '🛍️',
  'email': '📧',
  'project-management': '📋',
  'social-media': '📱',
  'storage-docs': '☁️',
  'customer-support': '🎧',
  'uncategorized': '📦',
  // Default icon for unknown categories
  'default': '📦'
};

/**
 * Generate a user-friendly display name from a category ID
 * @param categoryId - The category ID from frontmatter (e.g., 'development-architecture')
 * @returns User-friendly display name (e.g., 'Development & Architecture')
 */
export function generateCategoryDisplayName(categoryId: string): string {
  return categoryId
    .split('-')
    .map(word => {
      // Check for special cases first
      const lowerWord = word.toLowerCase();
      if (SPECIAL_CASES[lowerWord]) {
        return SPECIAL_CASES[lowerWord];
      }
      
      // Otherwise, capitalize first letter
      return word.charAt(0).toUpperCase() + word.slice(1).toLowerCase();
    })
    .join(' & ');
}

/**
 * Get icon for a category
 * @param categoryId - The category ID
 * @returns Icon emoji for the category
 */
export function getCategoryIcon(categoryId: string): string {
  return CATEGORY_ICONS[categoryId] || CATEGORY_ICONS.default;
}

/**
 * Category metadata interface
 */
export interface CategoryMetadata {
  id: string;
  displayName: string;
  icon: string;
  count: number;
}

/**
 * Generate category metadata from a list of categories with counts
 */
export function generateCategoryMetadata(
  categoryCounts: Record<string, number>
): CategoryMetadata[] {
  return Object.entries(categoryCounts)
    .map(([id, count]) => ({
      id,
      displayName: generateCategoryDisplayName(id),
      icon: getCategoryIcon(id),
      count
    }))
    .sort((a, b) => a.displayName.localeCompare(b.displayName));
}