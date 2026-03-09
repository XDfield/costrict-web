import { NextRequest, NextResponse } from 'next/server'
import { db } from '@/lib/db/client'
import { customResources, resourceVersions, organizations, organizationMembers } from '@/lib/db/enterprise-schema'
import { eq, and, desc, asc } from 'drizzle-orm'
import { auth } from '@/lib/auth'

// GET /api/enterprise/resources - List resources
export async function GET(request: NextRequest) {
  try {
    // Get authenticated user
    const user = await auth(request)
    if (!user) {
      return NextResponse.json({ error: 'Unauthorized' }, { status: 401 })
    }

    const searchParams = request.nextUrl.searchParams
    const type = searchParams.get('type') as 'mcp_server' | 'agent' | 'command' | 'hook' | 'skill' | 'plugin' | null
    const visibility = searchParams.get('visibility') as 'private' | 'organization' | 'public' | null
    const status = searchParams.get('status') as 'draft' | 'published' | 'archived' | null
    const page = parseInt(searchParams.get('page') || '1')
    const limit = parseInt(searchParams.get('limit') || '20')
    const offset = (page - 1) * limit

    // Build query conditions
    const conditions = []

    // Filter by type
    if (type) {
      conditions.push(eq(customResources.type, type))
    }

    // Filter by visibility
    if (visibility) {
      conditions.push(eq(customResources.visibility, visibility))
    }

    // Filter by status
    if (status) {
      conditions.push(eq(customResources.status, status))
    }

    // Get user's organizations
    const userOrgs = await db.query.organizationMembers.findMany({
      where: eq(organizationMembers.userId, user.id),
      with: {
        organization: true
      }
    })

    const orgIds = userOrgs.map(m => m.organizationId)

    // Filter by organization access
    conditions.push(
      orgIds.length > 0 
        ? eq(customResources.organizationId, orgIds[0]) // Simplified: first org
        : eq(customResources.organizationId, '') // No access
    )

    // Fetch resources
    const resources = await db.query.customResources.findMany({
      where: conditions.length > 0 ? and(...conditions) : undefined,
      orderBy: [desc(customResources.createdAt)],
      limit,
      offset,
      with: {
        createdBy: {
          columns: {
            id: true,
            name: true,
            email: true,
            avatarUrl: true
          }
        },
        updatedBy: {
          columns: {
            id: true,
            name: true
          }
        }
      }
    })

    // Get total count
    const totalCount = await db.query.customResources.findMany({
      where: conditions.length > 0 ? and(...conditions) : undefined
    })

    return NextResponse.json({
      resources,
      pagination: {
        page,
        limit,
        total: totalCount.length,
        pages: Math.ceil(totalCount.length / limit)
      }
    })
  } catch (error) {
    console.error('Error fetching resources:', error)
    return NextResponse.json({ error: 'Internal server error' }, { status: 500 })
  }
}

// POST /api/enterprise/resources - Create resource
export async function POST(request: NextRequest) {
  try {
    // Get authenticated user
    const user = await auth(request)
    if (!user) {
      return NextResponse.json({ error: 'Unauthorized' }, { status: 401 })
    }

    const body = await request.json()

    // Validate required fields
    if (!body.name || !body.type || !body.content) {
      return NextResponse.json(
        { error: 'Missing required fields: name, type, content' },
        { status: 400 }
      )
    }

    // Get user's organization
    const userOrg = await db.query.organizationMembers.findFirst({
      where: eq(organizationMembers.userId, user.id),
      with: {
        organization: true
      }
    })

    if (!userOrg) {
      return NextResponse.json({ error: 'No organization found' }, { status: 403 })
    }

    // Check organization limits
    const org = userOrg.organization
    const currentResourceCount = await db.query.customResources.findMany({
      where: eq(customResources.organizationId, org.id)
    })

    if (currentResourceCount.length >= (org.maxResources || 100)) {
      return NextResponse.json(
        { error: 'Organization resource limit reached' },
        { status: 403 }
      )
    }

    // Generate slug
    const slug = body.slug || `${body.type}-${Date.now()}`

    // Create resource
    const [newResource] = await db.insert(customResources).values({
      organizationId: org.id,
      createdBy: user.id,
      updatedBy: user.id,
      name: body.name,
      slug,
      type: body.type,
      category: body.category,
      content: body.content,
      config: body.config,
      description: body.description,
      tags: body.tags || [],
      version: body.version || '1.0.0',
      visibility: body.visibility || 'private',
      deploymentConfig: body.deploymentConfig,
      status: body.status || 'draft',
      active: true,
      featured: false
    }).returning()

    // Create initial version
    await db.insert(resourceVersions).values({
      resourceId: newResource.id,
      version: body.version || '1.0.0',
      versionNumber: 1,
      content: body.content,
      config: body.config,
      changeLog: 'Initial version',
      changedBy: user.id,
      changeType: 'created'
    })

    return NextResponse.json({ resource: newResource }, { status: 201 })
  } catch (error) {
    console.error('Error creating resource:', error)
    return NextResponse.json({ error: 'Internal server error' }, { status: 500 })
  }
}