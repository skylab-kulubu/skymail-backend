package handlers

import (
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

type ListHandler interface {
	CreateList(c fiber.Ctx) error
	GetLists(c fiber.Ctx) error
	GetList(c fiber.Ctx) error
	UpdateList(c fiber.Ctx) error
	DeleteList(c fiber.Ctx) error
	AddRecipient(c fiber.Ctx) error
	RemoveRecipient(c fiber.Ctx) error
	GetRecipients(c fiber.Ctx) error
}

type listHandlerImpl struct {
	db *database.Store
}

func NewListHandler(db *database.Store) ListHandler {
	return &listHandlerImpl{
		db: db,
	}
}

// CreateList godoc
//
// @Summary Create a new mailing list
// @Description Create a new mailing list with the provided name.
// @Tags lists
// @Accept json
// @Produce json
// @Param list body requests.CreateMailingList true "List details"
// @Success 204 "No Content"
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /lists [post]
func (h *listHandlerImpl) CreateList(c fiber.Ctx) error {
	var params requests.CreateMailingList

	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	_, err := h.db.CreateMailingList(c.Context(), params.Name)
	if err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// GetLists godoc
//
// @Summary List all mailing lists
// @Description Get a list of all mailing lists.
// @Tags lists
// @Produce json
// @Success 200 {array} database.MailingList
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /lists [get]
func (h *listHandlerImpl) GetLists(c fiber.Ctx) error {
	lists, err := h.db.GetAllMailingLists(c.Context())
	if err != nil {
		return err
	}

	return c.JSON(lists)
}

// GetList godoc
//
// @Summary Get a mailing list by ID
// @Description Get details of a specific mailing list by its ID.
// @Tags lists
// @Produce json
// @Param id path string true "List ID"
// @Success 200 {object} database.MailingList
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 404 {object} apperrors.AppError "Not Found"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /lists/{id} [get]
func (h *listHandlerImpl) GetList(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	list, err := h.db.GetMailingListById(c.Context(), id)
	if err != nil {
		return err
	}

	return c.JSON(list)
}

// UpdateList godoc
//
// @Summary Update a mailing list
// @Description Update an existing mailing list with the provided ID and details.
// @Tags lists
// @Accept json
// @Produce json
// @Param id path string true "List ID"
// @Param list body requests.UpdateMailingList true "List details"
// @Success 204 "No Content"
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 404 {object} apperrors.AppError "Not Found"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /lists/{id} [put]
func (h *listHandlerImpl) UpdateList(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	var params requests.UpdateMailingList
	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	_, err = h.db.UpdateMailingList(c.Context(), database.UpdateMailingListParams{
		ID:   id,
		Name: params.Name,
	})
	if err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// DeleteList godoc
//
// @Summary Delete a mailing list
// @Description Delete an existing mailing list by its ID.
// @Tags lists
// @Produce json
// @Param id path string true "List ID"
// @Success 204 "No Content"
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 404 {object} apperrors.AppError "Not Found"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /lists/{id} [delete]
func (h *listHandlerImpl) DeleteList(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	if err := h.db.DeleteMailingList(c.Context(), id); err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// AddRecipient godoc
//
// @Summary Add a recipient to a mailing list
// @Description Add a new recipient or link an existing one by email to a mailing list.
// @Tags lists
// @Accept json
// @Produce json
// @Param id path string true "List ID"
// @Param recipient body requests.AddRecipient true "Recipient details"
// @Success 204 "No Content"
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /lists/{id}/recipients [post]
func (h *listHandlerImpl) AddRecipient(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	var params requests.AddRecipient
	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	err = h.db.AddRecipientToMailingList(c.Context(), database.AddRecipientToMailingListParams{
		MailListID: id,
		FullName:   params.FullName,
		Email:      params.Email,
	})
	if err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// RemoveRecipient godoc
//
// @Summary Remove a recipient from a mailing list
// @Description Remove a specific recipient from a mailing list by their IDs.
// @Tags lists
// @Produce json
// @Param id path string true "List ID"
// @Param recipientId path string true "Recipient ID"
// @Success 204 "No Content"
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /lists/{id}/recipients/{recipientId} [delete]
func (h *listHandlerImpl) RemoveRecipient(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	recipientId, err := uuid.Parse(c.Params("recipientId"))
	if err != nil {
		return err
	}

	err = h.db.RemoveRecipientFromMailingListByID(c.Context(), database.RemoveRecipientFromMailingListByIDParams{
		MailListID:  id,
		RecipientID: recipientId,
	})
	if err != nil {
		return err
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// GetRecipients godoc
//
// @Summary List recipients of a mailing list
// @Description Get a list of all recipients in a specific mailing list.
// @Tags lists
// @Produce json
// @Param id path string true "List ID"
// @Success 200 {array} database.Recipient
// @Failure 400 {object} apperrors.AppError "Bad Request"
// @Failure 500 {object} apperrors.AppError "Internal Server Error"
// @Router /lists/{id}/recipients [get]
func (h *listHandlerImpl) GetRecipients(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	recipients, err := h.db.GetRecipientsByMailingListId(c.Context(), id)
	if err != nil {
		return err
	}

	return c.JSON(recipients)
}
