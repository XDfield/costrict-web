package llm

import "fmt"

// Prompt templates for skill generation and management

// Skill generation prompts
const (
	// SystemPromptSkillGenerate is the system prompt for skill generation
	SystemPromptSkillGenerate = `You are an expert AI assistant that helps create structured skill definitions for AI coding assistants.

Given a user's description or use case, generate a well-structured skill definition that includes:
1. A clear, concise name
2. A detailed description of what the skill does
3. The appropriate item type (skill, prompt, or tool)
4. The skill content (markdown for prompts, JSON for tools)
5. Relevant tags and categories

Output should be valid JSON with the following structure:
{
  "name": "Skill Name",
  "slug": "skill-name-slug",
  "description": "Detailed description of the skill",
  "itemType": "skill|prompt|tool",
  "category": "Category name",
  "content": "The actual skill content",
  "metadata": {
    "tags": ["tag1", "tag2"],
    "examples": ["Example usage 1"],
    "prerequisites": ["Required dependencies"],
    "version": "1.0.0"
  }
}

Focus on creating reusable, well-documented skills that can be easily understood and used by AI assistants.`

	// UserPromptSkillGenerate is the user prompt template for skill generation
	UserPromptSkillGenerate = `Generate a skill definition based on the following request:

Request: %s

Additional Context:
%s

Please generate a complete skill definition in JSON format.`

	// SystemPromptSkillImprove is the system prompt for skill improvement
	SystemPromptSkillImprove = `You are an expert AI assistant that helps improve existing skill definitions.

Analyze the given skill and suggest improvements based on:
1. Clarity and completeness of the description
2. Effectiveness of the content
3. Best practices for the skill type
4. Common use cases and edge cases

Output should be valid JSON with the following structure:
{
  "improvements": [
    {
      "field": "description|content|metadata",
      "current": "current value",
      "suggested": "suggested improvement",
      "reason": "why this change is recommended"
    }
  ],
  "overallScore": 0-100,
  "strengths": ["What the skill does well"],
  "weaknesses": ["Areas for improvement"]
}`

	// UserPromptSkillImprove is the user prompt template for skill improvement
	UserPromptSkillImprove = `Analyze and suggest improvements for this skill:

Name: %s
Type: %s
Description: %s
Content: %s
Metadata: %s

Please provide improvement suggestions in JSON format.`
)

// Search and recommendation prompts
const (
	// SystemPromptQueryExpansion is used to expand user search queries
	SystemPromptQueryExpansion = `You are a search query optimization assistant.

Given a user's search query, generate:
1. Alternative search terms that might find relevant results
2. Related concepts and synonyms
3. Potential categories or types of skills that might be relevant

Output should be valid JSON:
{
  "expandedTerms": ["term1", "term2"],
  "synonyms": ["synonym1", "synonym2"],
  "relatedConcepts": ["concept1", "concept2"],
  "suggestedCategories": ["category1", "category2"],
  "searchIntent": "brief description of what the user is looking for"
}`

	// UserPromptQueryExpansion is the user prompt for query expansion
	UserPromptQueryExpansion = `Expand this search query to find relevant skills:

Query: %s

Generate expanded search terms and related concepts.`
)

// BuildSkillGeneratePrompt builds the skill generation prompt
func BuildSkillGeneratePrompt(request, context string) (string, string) {
	systemPrompt := SystemPromptSkillGenerate
	userPrompt := fmt.Sprintf(UserPromptSkillGenerate, request, context)
	return systemPrompt, userPrompt
}

// BuildSkillImprovePrompt builds the skill improvement prompt
func BuildSkillImprovePrompt(name, itemType, description, content, metadata string) (string, string) {
	systemPrompt := SystemPromptSkillImprove
	userPrompt := fmt.Sprintf(UserPromptSkillImprove, name, itemType, description, content, metadata)
	return systemPrompt, userPrompt
}

// BuildQueryExpansionPrompt builds the query expansion prompt
func BuildQueryExpansionPrompt(query string) (string, string) {
	systemPrompt := SystemPromptQueryExpansion
	userPrompt := fmt.Sprintf(UserPromptQueryExpansion, query)
	return systemPrompt, userPrompt
}
