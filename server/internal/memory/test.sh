#!/bin/bash
# Memory Module API Test Script
# Usage: ./test.sh <JWT_TOKEN>
# Get JWT token from: http://localhost:8080/swagger/index.html -> /api/auth/login -> copy token from response

set -e

TOKEN="${1:-}"
BASE_URL="http://localhost:8080/api"

if [ -z "$TOKEN" ]; then
    echo "Usage: ./test.sh <JWT_TOKEN>"
    echo ""
    echo "To get a JWT token:"
    echo "  1. Start the server (see below)"
    echo "  2. Open http://localhost:8080/swagger/index.html"
    echo "  3. Use POST /api/auth/login or go through OAuth flow"
    echo "  4. Copy the access_token from the response"
    echo ""
    echo "Quick start:"
    echo "  cd /path/to/project && docker-compose up -d postgres redis"
    echo "  cd server && source .env && go run cmd/api/main.go"
    exit 1
fi

echo "=== Memory API Test Suite ==="
echo ""

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

request() {
    local method=$1
    local path=$2
    local body=$3
    local desc=$4

    echo "[$method] $path"
    echo "  $desc"

    if [ -n "$body" ]; then
        response=$(curl -s -w "\n%{http_code}" -X "$method" \
            -H "Authorization: Bearer $TOKEN" \
            -H "Content-Type: application/json" \
            -d "$body" \
            "$BASE_URL$path")
    else
        response=$(curl -s -w "\n%{http_code}" -X "$method" \
            -H "Authorization: Bearer $TOKEN" \
            "$BASE_URL$path")
    fi

    http_code=$(echo "$response" | tail -n1)
    body_json=$(echo "$response" | sed '$d')

    if [ "$http_code" -ge 200 ] && [ "$http_code" -lt 300 ]; then
        echo -e "  ${GREEN}OK ($http_code)${NC}"
    else
        echo -e "  ${RED}FAIL ($http_code)${NC}"
    fi
    echo "  Response: $body_json"
    echo ""

    # Return body for chaining (extract ID etc.)
    echo "$body_json"
}

# 1. Create a memory
echo "--- 1. Create Memory ---"
CREATE_RESP=$(request "POST" "/memories" '{
    "name": "User language preference",
    "slug": "user_language",
    "projectPath": "/Users/linkai/code/csc",
    "workDir": "/Users/linkai/code/csc",
    "type": "user",
    "description": "User communicates in Chinese and prefers Chinese responses",
    "content": "---\nname: User language preference\ndescription: User communicates in Chinese and prefers Chinese responses\ntype: user\n---\n\nThe user writes in Chinese (Mandarin) and expects responses in Chinese.\n\n**How to apply:** Default to Chinese for all conversational responses, explanations, and summaries."
}' "Create a new user-type memory")

# Extract memory ID from response (naive jq-like extraction)
MEMORY_ID=$(echo "$CREATE_RESP" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
if [ -z "$MEMORY_ID" ]; then
    echo "Failed to extract memory ID. Response:"
    echo "$CREATE_RESP"
    exit 1
fi
echo "  Created memory ID: $MEMORY_ID"
echo ""

# 2. List memories
echo "--- 2. List Memories ---"
request "GET" "/memories?projectPath=/Users/linkai/code/csc" "" "List memories filtered by project path"

# 3. Get memory detail
echo "--- 3. Get Memory Detail ---"
request "GET" "/memories/$MEMORY_ID" "" "Get memory metadata"

# 4. Get memory content
echo "--- 4. Get Memory Content ---"
request "GET" "/memories/$MEMORY_ID/content" "" "Get current version markdown content"

# 5. Update memory (create new version)
echo "--- 5. Update Memory (bumpVersion=true) ---"
request "PUT" "/memories/$MEMORY_ID" '{
    "name": "User language preference (updated)",
    "description": "Updated description",
    "content": "---\nname: User language preference (updated)\ndescription: Updated description\ntype: user\n---\n\nUpdated content here.\n\n**How to apply:** Always respond in Chinese.",
    "bumpVersion": true
}' "Update memory and create version 2"

# 6. Get version list
echo "--- 6. List Versions ---"
request "GET" "/memories/$MEMORY_ID/versions" "" "List version history"

# 7. Get v1 content
echo "--- 7. Get Version 1 Content ---"
request "GET" "/memories/$MEMORY_ID/versions/1/content" "" "Get version 1 markdown content"

# 8. Get v2 content (current)
echo "--- 8. Get Version 2 Content ---"
request "GET" "/memories/$MEMORY_ID/versions/2/content" "" "Get version 2 markdown content"

# 9. Update without bumping version
echo "--- 9. Update Memory (bumpVersion=false) ---"
request "PUT" "/memories/$MEMORY_ID" '{
    "content": "---\nname: User language preference (updated)\ndescription: Updated description\ntype: user\n---\n\nUpdated content without version bump.",
    "bumpVersion": false
}' "Update memory in-place (overwrite current version)"

# 10. Create another memory (feedback type)
echo "--- 10. Create Feedback Memory ---"
request "POST" "/memories" '{
    "name": "Mermaid syntax pitfalls",
    "slug": "feedback_mermaid_syntax",
    "projectPath": "/Users/linkai/code/csc",
    "type": "feedback",
    "description": "Node identifiers with special chars cause Mermaid parse errors",
    "content": "---\nname: Mermaid syntax pitfalls\ndescription: Node identifiers with special chars cause Mermaid parse errors\ntype: feedback\n---\n\nNode identifiers containing @, /, or * cause Mermaid parser errors.\n\n**Why:** Mermaid treats these as special tokens.\n\n**How to apply:** Use simple alphanumeric labels or quoted text."
}' "Create a feedback-type memory"

# 11. List with keyword search
echo "--- 11. Search Memories ---"
request "GET" "/memories?keyword=mermaid" "" "Search memories by keyword"

# 12. List with type filter
echo "--- 12. Filter by Type ---"
request "GET" "/memories?type=feedback" "" "Filter memories by type"

# 13. Delete memory
echo "--- 13. Delete Memory ---"
request "DELETE" "/memories/$MEMORY_ID" "" "Soft delete the memory"

# 14. Verify deletion (should 404)
echo "--- 14. Verify Deletion ---"
request "GET" "/memories/$MEMORY_ID" "" "Get deleted memory (expect 404)"

echo "=== Test Suite Complete ==="
