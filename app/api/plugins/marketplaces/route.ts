import { NextResponse } from 'next/server'
import { getPluginMarketplaces } from '@/lib/plugin-db-server'

export const dynamic = 'force-dynamic'

/**
 * GET /api/plugins/marketplaces
 * Get list of marketplaces for filter dropdown
 */
export async function GET() {
  try {
    const marketplaces = await getPluginMarketplaces()

    return NextResponse.json({ marketplaces })
  } catch (error) {
    console.error('Error fetching plugin marketplaces:', error)
    return NextResponse.json(
      { error: 'Failed to fetch marketplaces' },
      { status: 500 }
    )
  }
}
