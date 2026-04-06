package handlers

import (
	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

type ApplicationHandler interface {
	CreateApplication(c fiber.Ctx) error
	GetApplications(c fiber.Ctx) error
	GetApplication(c fiber.Ctx) error
	UpdateApplication(c fiber.Ctx) error
	DeleteApplication(c fiber.Ctx) error
	RerollToken(c fiber.Ctx) error
}

type applicationHandlerImpl struct {
	db        *database.Store
	appSecret []byte
}

func NewApplicationHandler(db *database.Store, appSecret string) ApplicationHandler {
	return &applicationHandlerImpl{
		db:        db,
		appSecret: []byte(appSecret),
	}
}

type ApplicationResponse struct {
	database.Application
	Token string `json:"token,omitempty"`
}

func (h *applicationHandlerImpl) generateToken(appID uuid.UUID, version int) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss":           "skymail",
		"app_id":        appID.String(),
		"token_version": version,
	})

	return token.SignedString(h.appSecret)
}

// CreateApplication godoc
//
//	@Summary		Create a new application
//	@Description	Create a new application and returns it with a generated token.
//	@Tags			Applications
//	@Accept			json
//	@Produce		json
//	@Param			application	body		requests.CreateApplication	true	"Application details"
//	@Success		201			{object}	handlers.ApplicationResponse
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		403			{object}	apperrors.AppError	"Forbidden"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/applications [post]
func (h *applicationHandlerImpl) CreateApplication(c fiber.Ctx) error {
	var params requests.CreateApplication

	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	ownerID, ok := c.Locals("user_id").(string)
	if !ok || ownerID == "" {
		return apperrors.ErrForbidden
	}

	app, err := h.db.CreateApplication(c.Context(), database.CreateApplicationParams{
		Name:    params.Name,
		OwnerID: ownerID,
	})
	if err != nil {
		return err
	}

	tokenStr, err := h.generateToken(app.ID, app.TokenVersion)
	if err != nil {
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(ApplicationResponse{
		Application: app,
		Token:       tokenStr,
	})
}

// GetApplications godoc
//
//	@Summary		List all applications
//	@Description	Get a list of all applications for the authenticated user.
//	@Tags			Applications
//	@Produce		json
//	@Success		200	{array}		database.Application
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/applications [get]
func (h *applicationHandlerImpl) GetApplications(c fiber.Ctx) error {
	ownerID, ok := c.Locals("user_id").(string)
	if !ok || ownerID == "" {
		return apperrors.ErrForbidden
	}

	apps, err := h.db.GetApplicationsByOwnerId(c.Context(), ownerID)
	if err != nil {
		return err
	}

	if apps == nil {
		apps = []database.Application{}
	}

	return c.JSON(apps)
}

// GetApplication godoc
//
//	@Summary		Get an application by ID
//	@Description	Get details of a specific application by its ID.
//	@Tags			Applications
//	@Produce		json
//	@Param			id	path		string	true	"Application ID"
//	@Success		200	{object}	database.Application
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/applications/{id} [get]
func (h *applicationHandlerImpl) GetApplication(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	ownerID, ok := c.Locals("user_id").(string)
	if !ok || ownerID == "" {
		return apperrors.ErrForbidden
	}

	app, err := h.db.GetApplicationByIdAndOwner(c.Context(), database.GetApplicationByIdAndOwnerParams{
		ID:      id,
		OwnerID: ownerID,
	})
	if err != nil {
		return err
	}

	return c.JSON(app)
}

// UpdateApplication godoc
//
//	@Summary		Update an application
//	@Description	Update an existing application.
//	@Tags			Applications
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string						true	"Application ID"
//	@Param			application	body		requests.UpdateApplication	true	"Application details"
//	@Success		200			{object}	database.Application
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		403			{object}	apperrors.AppError	"Forbidden"
//	@Failure		404			{object}	apperrors.AppError	"Not Found"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/applications/{id} [patch]
func (h *applicationHandlerImpl) UpdateApplication(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	var params requests.UpdateApplication
	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	ownerID, ok := c.Locals("user_id").(string)
	if !ok || ownerID == "" {
		return apperrors.ErrForbidden
	}

	updatedApp, err := h.db.UpdateApplication(c.Context(), database.UpdateApplicationParams{
		ID:      id,
		Name:    params.Name,
		OwnerID: ownerID,
	})
	if err != nil {
		return err
	}

	return c.JSON(updatedApp)
}

// DeleteApplication godoc
//
//	@Summary		Delete an application
//	@Description	Delete an existing application.
//	@Tags			Applications
//	@Produce		json
//	@Param			id	path	string	true	"Application ID"
//	@Success		204	"No Content"
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/applications/{id} [delete]
func (h *applicationHandlerImpl) DeleteApplication(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	ownerID, ok := c.Locals("user_id").(string)
	if !ok || ownerID == "" {
		return apperrors.ErrForbidden
	}

	if err := h.db.DeleteApplication(c.Context(), database.DeleteApplicationParams{
		ID:      id,
		OwnerID: ownerID,
	}); err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// RerollToken godoc
//
//	@Summary		Reroll application token
//	@Description	Invalidate old tokens and generate a new one.
//	@Tags			Applications
//	@Produce		json
//	@Param			id	path		string	true	"Application ID"
//	@Success		200	{object}	handlers.ApplicationResponse
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		403	{object}	apperrors.AppError	"Forbidden"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/applications/{id}/reroll [post]
func (h *applicationHandlerImpl) RerollToken(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	ownerID, ok := c.Locals("user_id").(string)
	if !ok || ownerID == "" {
		return apperrors.ErrForbidden
	}

	updatedApp, err := h.db.RerollApplicationToken(c.Context(), database.RerollApplicationTokenParams{
		ID:      id,
		OwnerID: ownerID,
	})
	if err != nil {
		return err
	}

	tokenStr, err := h.generateToken(updatedApp.ID, updatedApp.TokenVersion)
	if err != nil {
		return err
	}

	return c.JSON(ApplicationResponse{
		Application: updatedApp,
		Token:       tokenStr,
	})
}
