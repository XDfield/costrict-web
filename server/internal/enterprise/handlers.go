package enterprise

import (
	"errors"
	"net/http"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

// customerResponse is the public shape returned to the frontend store. AccountIDs
// (jsonb) is decoded into a flat `ids` string array so the client can run
// matchEnterprise(item.createdBy) === ids.includes(createdBy).
type customerResponse struct {
	ID   string   `json:"id"`
	IDs  []string `json:"ids"`
	Name string   `json:"name"`
	Logo string   `json:"logo"`
}

type customerRequest struct {
	Name string   `json:"name" binding:"required"`
	Logo string   `json:"logo" binding:"required"`
	IDs  []string `json:"ids"`
}

// ListEnterpriseCustomersHandler godoc
// @Summary      List enterprise customers
// @Description  List all enterprise customer branding configs (readable by any authenticated user)
// @Tags         enterprise-customers
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  object{customers=[]object{id=string,ids=[]string,name=string,logo=string}}
// @Failure      401  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /enterprise-customers [get]
func ListEnterpriseCustomersHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		customers, err := svc.List()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list enterprise customers"})
			return
		}

		out := make([]customerResponse, 0, len(customers))
		for _, customer := range customers {
			out = append(out, customerResponse{
				ID:   customer.ID,
				IDs:  decodeIDs(customer.AccountIDs),
				Name: customer.Name,
				Logo: customer.Logo,
			})
		}
		c.JSON(http.StatusOK, gin.H{"customers": out})
	}
}

// CreateEnterpriseCustomerHandler godoc
// @Summary      Create enterprise customer
// @Description  Create an enterprise customer branding config (platform admin only)
// @Tags         admin/enterprise-customers
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      object{name=string,logo=string,ids=[]string}  true  "Enterprise customer"
// @Success      200   {object}  object{customer=object}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /admin/enterprise-customers [post]
func CreateEnterpriseCustomerHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req customerRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		customer, err := svc.Create(req.Name, req.Logo, req.IDs, operatorID)
		if err != nil {
			switch {
			case errors.Is(err, ErrInvalidEnterpriseCustomer):
				c.JSON(http.StatusBadRequest, gin.H{"error": "name and logo are required"})
			case errors.Is(err, ErrLogoTooLarge):
				c.JSON(http.StatusBadRequest, gin.H{"error": "logo too large or not an image data uri"})
			case errors.Is(err, ErrNameTooLong):
				c.JSON(http.StatusBadRequest, gin.H{"error": "name too long"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create enterprise customer"})
			}
			return
		}

		c.JSON(http.StatusOK, gin.H{"customer": customerResponse{
			ID:   customer.ID,
			IDs:  decodeIDs(customer.AccountIDs),
			Name: customer.Name,
			Logo: customer.Logo,
		}})
	}
}

// UpdateEnterpriseCustomerHandler godoc
// @Summary      Update enterprise customer
// @Description  Update an enterprise customer branding config (platform admin only)
// @Tags         admin/enterprise-customers
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                                          true  "Enterprise customer ID"
// @Param        body  body      object{name=string,logo=string,ids=[]string}  true  "Enterprise customer"
// @Success      200   {object}  object{customer=object}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /admin/enterprise-customers/{id} [put]
func UpdateEnterpriseCustomerHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req customerRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		customer, err := svc.Update(c.Param("id"), req.Name, req.Logo, req.IDs)
		if err != nil {
			switch {
			case errors.Is(err, ErrInvalidEnterpriseCustomer):
				c.JSON(http.StatusBadRequest, gin.H{"error": "name and logo are required"})
			case errors.Is(err, ErrLogoTooLarge):
				c.JSON(http.StatusBadRequest, gin.H{"error": "logo too large or not an image data uri"})
			case errors.Is(err, ErrNameTooLong):
				c.JSON(http.StatusBadRequest, gin.H{"error": "name too long"})
			case errors.Is(err, ErrEnterpriseCustomerNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "enterprise customer not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update enterprise customer"})
			}
			return
		}

		c.JSON(http.StatusOK, gin.H{"customer": customerResponse{
			ID:   customer.ID,
			IDs:  decodeIDs(customer.AccountIDs),
			Name: customer.Name,
			Logo: customer.Logo,
		}})
	}
}

// DeleteEnterpriseCustomerHandler godoc
// @Summary      Delete enterprise customer
// @Description  Delete an enterprise customer branding config (platform admin only)
// @Tags         admin/enterprise-customers
// @Produce      json
// @Security     BearerAuth
// @Param        id  path      string  true  "Enterprise customer ID"
// @Success      200  {object}  object{success=bool}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /admin/enterprise-customers/{id} [delete]
func DeleteEnterpriseCustomerHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		if err := svc.Delete(c.Param("id")); err != nil {
			if errors.Is(err, ErrEnterpriseCustomerNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "enterprise customer not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete enterprise customer"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}
