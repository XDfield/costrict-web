package collaboration

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (m *Module) listIssuesHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID := getSpaceID(c)
		status := c.Query("status")
		priority := c.Query("priority")
		assigneeID := c.Query("assigneeId")
		issues, err := m.service.ListIssues(spaceID, status, priority, assigneeID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, IssuesResponse{Issues: issues})
	}
}

func (m *Module) createIssueHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateIssueRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		spaceID := getSpaceID(c)
		issue, err := m.service.CreateIssue(spaceID, currentUserID(c), &req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, IssueResponse{Issue: issue})
	}
}

func (m *Module) getIssueHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID := getSpaceID(c)
		issueID := c.Param("id")
		issue, err := m.service.GetIssue(spaceID, issueID)
		if err != nil {
			writeError(c, err)
			return
		}
		comments, _ := m.service.ListComments(issue.ID)
		c.JSON(http.StatusOK, IssueResponse{Issue: issue, Comments: comments})
	}
}

func (m *Module) updateIssueHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req UpdateIssueRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		spaceID := getSpaceID(c)
		issueID := c.Param("id")
		issue, err := m.service.UpdateIssue(spaceID, issueID, &req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, IssueResponse{Issue: issue})
	}
}

func (m *Module) deleteIssueHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		spaceID := getSpaceID(c)
		issueID := c.Param("id")
		if err := m.service.DeleteIssue(spaceID, issueID); err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusNoContent, gin.H{})
	}
}

func (m *Module) listCommentsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		issueID := c.Param("id")
		comments, err := m.service.ListComments(issueID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, CommentsResponse{Comments: comments})
	}
}

func (m *Module) createCommentHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req CreateCommentRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		issueID := c.Param("id")
		comment, err := m.service.CreateComment(issueID, currentUserID(c), &req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, comment)
	}
}
