package rest

import (
	"net/http"
	"sync/atomic"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/labstack/echo/v4"
)

// ready reports whether the framework has finished initializing and may accept
// traffic. It is set by Run and read by the readiness handler.
var ready atomic.Bool

// markReady records the readiness state reported by GET /aiha/readyz.
func markReady(v bool) {
	ready.Store(v)
}

// RestGetReadyz godoc
// @ID GetReadyz
// @Summary Check whether the framework is ready
// @Description Report whether the AI-HA framework has completed initialization and can accept requests.
// @Tags [Admin] System Management
// @Produce json
// @Success 200 {object} model.SimpleMsg
// @Failure 503 {object} model.SimpleMsg
// @Router /readyz [get]
func RestGetReadyz(c echo.Context) error {
	if !ready.Load() {
		return c.JSON(http.StatusServiceUnavailable, model.SimpleMsg{Message: "Service initializing, retry shortly"})
	}
	return c.JSON(http.StatusOK, model.SimpleMsg{Message: "Service ready"})
}

// RestGetSwaggerSpec godoc
// @ID GetSwaggerSpec
// @Summary Get the OpenAPI specification
// @Description Return the generated OpenAPI (Swagger) document describing this API.
// @Tags [Admin] System Management
// @Produce json
// @Success 200 {object} object
// @Router /swagger.json [get]
func RestGetSwaggerSpec(c echo.Context) error {
	return c.JSONBlob(http.StatusOK, swaggerSpec)
}
