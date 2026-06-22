package enterprise

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/audit"
	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
)

// customerResponse is the PUBLIC shape returned to the frontend store (readable
// by any authenticated user). `ids` is the RESOLVED subject_id list (the stored
// account_ids are universal_id, resolved here via ResolveSubjectIDs) so the client
// can keep running matchEnterprise(item.createdBy) === ids.includes(createdBy).
// universal_id and member identities are deliberately NEVER exposed here — that
// would leak who is in each enterprise to every logged-in user.
type customerResponse struct {
	ID   string   `json:"id"`
	IDs  []string `json:"ids"`
	Name string   `json:"name"`
	Logo string   `json:"logo"`
}

// adminCustomerResponse is the platform-admin shape: it returns the raw stored
// universal_id list plus resolved Member rows (username/displayName/avatarUrl) so
// the admin console can show "who is configured" and pre-fill the edit form. Only
// served behind RequirePlatformAdmin.
type adminCustomerResponse struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Logo         string   `json:"logo"`
	UniversalIDs []string `json:"universalIds"`
	Members      []Member `json:"members"`
}

type customerRequest struct {
	Name string   `json:"name" binding:"required"`
	Logo string   `json:"logo" binding:"required"`
	IDs  []string `json:"ids"` // Casdoor universal_id list
}

// toAdminResponse builds the admin shape from a stored customer, resolving its
// universal_id account list into Member rows via a shared byUniversalID map (so
// the whole list resolves in one batch query — see ListEnterpriseCustomersAdminHandler).
// universalIDs preserves stored order.
func toAdminResponse(customer *models.EnterpriseCustomer, byUniversalID map[string]Member) adminCustomerResponse {
	universalIDs := decodeIDs(customer.AccountIDs)
	return adminCustomerResponse{
		ID:           customer.ID,
		Name:         customer.Name,
		Logo:         customer.Logo,
		UniversalIDs: universalIDs,
		Members:      assembleMembers(universalIDs, byUniversalID),
	}
}

// collectUniversalIDs gathers the union of every customer's stored account_ids
// (universal_id values) so the whole list can be resolved in one batch query
// rather than one query per row (avoids N+1 on the list endpoints).
func collectUniversalIDs(customers []models.EnterpriseCustomer) []string {
	all := make([]string, 0, len(customers))
	for i := range customers {
		all = append(all, decodeIDs(customers[i].AccountIDs)...)
	}
	return all
}

// adminResponseFor builds the admin shape for a SINGLE customer (create/update
// echo-back). It resolves that customer's universal_id list with one batch query.
func adminResponseFor(svc *Service, customer *models.EnterpriseCustomer) adminCustomerResponse {
	universalIDs := decodeIDs(customer.AccountIDs)
	return toAdminResponse(customer, svc.ResolveMembersBatch(universalIDs))
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

		// Resolve every customer's universal_id list in a single batch query, then
		// assemble each row from the shared map (O(1) DB round-trips, not N+1). This
		// matters because this endpoint is PUBLIC (any authenticated user) and was
		// otherwise a per-customer query amplification vector.
		byUniversalID := svc.ResolveMembersBatch(collectUniversalIDs(customers))

		out := make([]customerResponse, 0, len(customers))
		for _, customer := range customers {
			out = append(out, customerResponse{
				ID:   customer.ID,
				IDs:  subjectIDsFrom(decodeIDs(customer.AccountIDs), byUniversalID),
				Name: customer.Name,
				Logo: customer.Logo,
			})
		}
		c.JSON(http.StatusOK, gin.H{"customers": out})
	}
}

// ListEnterpriseCustomersAdminHandler godoc
// @Summary      List enterprise customers (admin)
// @Description  List all enterprise customer configs with raw universal_id account list + resolved members (platform admin only)
// @Tags         admin/enterprise-customers
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  object{customers=[]object{id=string,name=string,logo=string,universalIds=[]string,members=[]object}}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /admin/enterprise-customers [get]
func ListEnterpriseCustomersAdminHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		customers, err := svc.List()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list enterprise customers"})
			return
		}

		// Batch-resolve the union of all customers' universal_ids in one query, then
		// assemble each row from the shared map (avoids N+1 across the list).
		byUniversalID := svc.ResolveMembersBatch(collectUniversalIDs(customers))

		out := make([]adminCustomerResponse, 0, len(customers))
		for i := range customers {
			out = append(out, toAdminResponse(&customers[i], byUniversalID))
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
// @Param        body  body      object{name=string,logo=string,ids=[]string}  true  "Enterprise customer (ids = Casdoor universal_id list)"
// @Success      200   {object}  object{customer=object{id=string,name=string,logo=string,universalIds=[]string,members=[]object}}
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
			case errors.Is(err, ErrLogoInvalid):
				c.JSON(http.StatusBadRequest, gin.H{"error": "logo is not a valid base64 image data uri"})
			case errors.Is(err, ErrNameTooLong):
				c.JSON(http.StatusBadRequest, gin.H{"error": "name too long"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create enterprise customer"})
			}
			return
		}

		// Audit detail records the raw universal_id list (the stored identity anchor).
		audit.Record(operatorID, audit.ActionEnterpriseCreate, audit.TargetEnterpriseCustomer, customer.ID, gin.H{
			"name": customer.Name,
			"ids":  decodeIDs(customer.AccountIDs),
		})

		c.JSON(http.StatusOK, gin.H{"customer": adminResponseFor(svc, customer)})
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
// @Param        body  body      object{name=string,logo=string,ids=[]string}  true  "Enterprise customer (ids = Casdoor universal_id list)"
// @Success      200   {object}  object{customer=object{id=string,name=string,logo=string,universalIds=[]string,members=[]object}}
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
			case errors.Is(err, ErrLogoInvalid):
				c.JSON(http.StatusBadRequest, gin.H{"error": "logo is not a valid base64 image data uri"})
			case errors.Is(err, ErrNameTooLong):
				c.JSON(http.StatusBadRequest, gin.H{"error": "name too long"})
			case errors.Is(err, ErrEnterpriseCustomerNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "enterprise customer not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update enterprise customer"})
			}
			return
		}

		// Audit detail records the raw universal_id list (the stored identity anchor).
		audit.Record(operatorID, audit.ActionEnterpriseUpdate, audit.TargetEnterpriseCustomer, customer.ID, gin.H{
			"name": customer.Name,
			"ids":  decodeIDs(customer.AccountIDs),
		})

		c.JSON(http.StatusOK, gin.H{"customer": adminResponseFor(svc, customer)})
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

		audit.Record(operatorID, audit.ActionEnterpriseDelete, audit.TargetEnterpriseCustomer, c.Param("id"), nil)

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}
