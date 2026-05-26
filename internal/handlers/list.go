package handlers

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

type MailingListItem struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Source      string    `json:"source"`
}

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
	kc keycloak.Client
}

func NewListHandler(db *database.Store, kc keycloak.Client) ListHandler {
	return &listHandlerImpl{db: db, kc: kc}
}

func dbListToItem(l database.MailingList) MailingListItem {
	return MailingListItem{
		ID: l.ID, Name: l.Name, Description: l.Description,
		CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt,
		Source: "internal",
	}
}

func kcGroupToItem(g *gocloak.Group) MailingListItem {
	id, _ := uuid.Parse(*g.ID)
	now := time.Now()
	path := *g.Path
	return MailingListItem{
		ID: id, Name: *g.Name, Description: &path,
		CreatedAt: now, UpdatedAt: now,
		Source: "keycloak",
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows)
}

func userFullName(u *gocloak.User) string {
	name := strings.TrimSpace(gocloak.PString(u.FirstName) + " " + gocloak.PString(u.LastName))
	if name == "" {
		return gocloak.PString(u.Username)
	}
	return name
}

// CreateList godoc
//
//	@Summary		Create a new mailing list
//	@Description	Create a new mailing list with the provided name.
//	@Tags			Lists
//	@Accept			json
//	@Produce		json
//	@Param			list	body		requests.CreateMailingList	true	"List details"
//	@Success		201		{object}	database.MailingList
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mailing_lists [post]
func (h *listHandlerImpl) CreateList(c fiber.Ctx) error {
	var params requests.CreateMailingList

	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	list, err := h.db.CreateMailingList(c.Context(), params.Name)
	if err != nil {
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(list)
}

// GetLists godoc
//
//	@Summary		List all mailing lists
//	@Description	Get a list of all mailing lists (internal + Keycloak groups) with pagination.
//	@Tags			Lists
//	@Produce		json
//	@Param			_start	query		int	false	"Start index"
//	@Param			_end	query		int	false	"End index"
//	@Success		200		{array}		handlers.MailingListItem
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mailing_lists [get]
func (h *listHandlerImpl) GetLists(c fiber.Ctx) error {
	limit, offset := getPaginationParams(c)

	lists, err := h.db.GetAllMailingLists(c.Context(), database.GetAllMailingListsParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return err
	}

	dbCount, err := h.db.CountMailingLists(c.Context())
	if err != nil {
		return err
	}

	groups, err := h.kc.ListGroups(c.Context())
	if err != nil {
		return err
	}

	items := make([]MailingListItem, 0, len(lists)+len(groups))
	for _, l := range lists {
		items = append(items, dbListToItem(l))
	}
	for _, g := range groups {
		items = append(items, kcGroupToItem(g))
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(dbCount+int64(len(groups)), 10))
	return c.JSON(items)
}

// GetList godoc
//
//	@Summary		Get a mailing list by ID
//	@Description	Get details of a specific mailing list by its ID. Falls back to Keycloak groups.
//	@Tags			Lists
//	@Produce		json
//	@Param			id	path		string	true	"List ID"
//	@Success		200	{object}	handlers.MailingListItem
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mailing_lists/{id} [get]
func (h *listHandlerImpl) GetList(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	list, err := h.db.GetMailingListById(c.Context(), id)
	if err != nil {
		if !isNotFound(err) {
			return err
		}
		group, err := h.kc.GetGroup(c.Context(), id.String())
		if err != nil {
			return err
		}
		if group == nil {
			return fiber.ErrNotFound
		}
		return c.JSON(kcGroupToItem(group))
	}

	return c.JSON(dbListToItem(list))
}

// UpdateList godoc
//
//	@Summary		Update a mailing list
//	@Description	Update an existing mailing list with the provided ID and details.
//	@Tags			Lists
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string						true	"List ID"
//	@Param			list	body		requests.UpdateMailingList	true	"List details"
//	@Success		200		{object}	database.MailingList
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		404		{object}	apperrors.AppError	"Not Found"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mailing_lists/{id} [patch]
func (h *listHandlerImpl) UpdateList(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	var params requests.UpdateMailingList
	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	list, err := h.db.UpdateMailingList(c.Context(), database.UpdateMailingListParams{
		ID:   id,
		Name: params.Name,
	})
	if err != nil {
		return err
	}

	return c.JSON(list)
}

// DeleteList godoc
//
//	@Summary		Delete a mailing list
//	@Description	Delete an existing mailing list by its ID.
//	@Tags			Lists
//	@Produce		json
//	@Param			id	path	string	true	"List ID"
//	@Success		204	"No Content"
//	@Failure		400	{object}	apperrors.AppError	"Bad Request"
//	@Failure		404	{object}	apperrors.AppError	"Not Found"
//	@Failure		500	{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mailing_lists/{id} [delete]
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
//	@Summary		Add a recipient to a mailing list
//	@Description	Add a new recipient or link an existing one by email to a mailing list.
//	@Tags			Lists
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string					true	"List ID"
//	@Param			recipient	body		requests.AddRecipient	true	"Recipient details"
//	@Success		201			{object}	database.Recipient
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mailing_lists/{id}/recipients [post]
func (h *listHandlerImpl) AddRecipient(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	var params requests.AddRecipient
	if err := c.Bind().Body(&params); err != nil {
		return err
	}

	recipient, err := h.db.AddRecipientToMailingList(c.Context(), database.AddRecipientToMailingListParams{
		MailListID: id,
		FullName:   params.FullName,
		Email:      params.Email,
	})
	if err != nil {
		return err
	}

	return c.Status(fiber.StatusCreated).JSON(recipient)
}

// RemoveRecipient godoc
//
//	@Summary		Remove a recipient from a mailing list
//	@Description	Remove a specific recipient from a mailing list by their IDs.
//	@Tags			Lists
//	@Produce		json
//	@Param			id			path	string	true	"List ID"
//	@Param			recipientId	path	string	true	"Recipient ID"
//	@Success		204			"No Content"
//	@Failure		400			{object}	apperrors.AppError	"Bad Request"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mailing_lists/{id}/recipients/{recipientId} [delete]
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
//	@Summary		List recipients of a mailing list
//	@Description	Get all recipients in a mailing list or Keycloak group with pagination.
//	@Tags			Lists
//	@Produce		json
//	@Param			id		path		string	true	"List ID"
//	@Param			_start	query		int		false	"Start index"
//	@Param			_end	query		int		false	"End index"
//	@Success		200		{array}		database.Recipient
//	@Failure		400		{object}	apperrors.AppError	"Bad Request"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/mailing_lists/{id}/recipients [get]
func (h *listHandlerImpl) GetRecipients(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return err
	}

	_, err = h.db.GetMailingListById(c.Context(), id)
	if err != nil {
		if !isNotFound(err) {
			return err
		}
		return h.getKeycloakGroupRecipients(c, id.String())
	}

	limit, offset := getPaginationParams(c)

	recipients, err := h.db.GetRecipientsByMailingListId(c.Context(), database.GetRecipientsByMailingListIdParams{
		MailListID: id,
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		return err
	}

	count, err := h.db.CountRecipientsByMailingListId(c.Context(), id)
	if err != nil {
		return err
	}

	c.Response().Header.Set("X-Total-Count", strconv.FormatInt(count, 10))
	return c.JSON(recipients)
}

func (h *listHandlerImpl) getKeycloakGroupRecipients(c fiber.Ctx, groupID string) error {
	members, err := h.kc.GetGroupMembers(c.Context(), groupID)
	if err != nil {
		return err
	}

	recipients := make([]database.Recipient, 0, len(members))
	now := time.Now()
	for _, m := range members {
		if gocloak.PString(m.Email) == "" {
			continue
		}
		uid, err := uuid.Parse(gocloak.PString(m.ID))
		if err != nil {
			continue
		}
		recipients = append(recipients, database.Recipient{
			ID:        uid,
			FullName:  userFullName(m),
			Email:     gocloak.PString(m.Email),
			CreatedAt: now,
			UpdatedAt: now,
		})
	}

	c.Response().Header.Set("X-Total-Count", strconv.Itoa(len(recipients)))
	return c.JSON(recipients)
}
