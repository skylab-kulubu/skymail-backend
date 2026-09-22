package handlers

import (
	"slices"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/internal/requiredvars"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// AddRequiredVariable godoc
//
//	@Summary		Mark a variable of a template required
//	@Description	Adds a variable to the template's operator Required variables: from then on every save and publish must keep referencing it. The published body must reference it already — a Required variable is a promise about the mail being sent — counted as the save check counts (inside a conditional section counts; a comment, plain text, a range or with element's field, or index . do not). A variable the contract set holds is required already and stays the contract's; asking for one, or for one operators marked, changes nothing. Changes no version.
//	@Tags			Templates
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string							true	"Template ID"
//	@Param			variable	body		requests.AddRequiredVariable	true	"The variable to require"
//	@Success		200			{object}	database.Template
//	@Failure		400			{object}	apperrors.AppError	"The name is not a variable name (validation.error)"
//	@Failure		403			{object}	apperrors.AppError	"Forbidden"
//	@Failure		404			{object}	apperrors.AppError	"No such template in use: unknown or archived"
//	@Failure		422			{object}	apperrors.AppError	"The published body does not reference the variable (template.required_variables_missing) or does not parse (template.body_unparseable)"
//	@Failure		500			{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id}/required-variables [post]
func (h *templateHandlerImpl) AddRequiredVariable(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}
	var params requests.AddRequiredVariable
	if err := c.Bind().Body(&params); err != nil {
		return err
	}
	if !validator.IsVariableName(params.Name) {
		return errInvalidVariableName
	}

	var template database.Template
	err = h.db.InTx(c.Context(), func(q *database.Queries) error {
		marked, err := q.AddOperatorRequiredVariable(c.Context(), database.AddOperatorRequiredVariableParams{ID: id, Name: params.Name})
		if err != nil {
			return err
		}
		template = marked
		return requiredvars.CheckBody(marked, marked.HtmlContent)
	})
	if err != nil {
		return err
	}
	return c.JSON(template)
}

// RemoveRequiredVariable godoc
//
//	@Summary		Release a variable operators marked required
//	@Description	Removes a variable from the template's operator Required variables. A contract variable — the sending service's, written by the Template seed — cannot be released here. Releasing a variable that is not required changes nothing. Changes no version.
//	@Tags			Templates
//	@Produce		json
//	@Param			id		path		string	true	"Template ID"
//	@Param			name	path		string	true	"Variable name"
//	@Success		200		{object}	database.Template
//	@Failure		400		{object}	apperrors.AppError	"The name is not a variable name (template.invalid_variable_name)"
//	@Failure		403		{object}	apperrors.AppError	"Forbidden"
//	@Failure		404		{object}	apperrors.AppError	"No such template in use: unknown or archived"
//	@Failure		409		{object}	apperrors.AppError	"The variable is in the contract set (template.required_variable_in_contract, params.name)"
//	@Failure		500		{object}	apperrors.AppError	"Internal Server Error"
//	@Router			/templates/{id}/required-variables/{name} [delete]
func (h *templateHandlerImpl) RemoveRequiredVariable(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return apperrors.ErrStatusNotFound
	}
	name := c.Params("name")
	if !validator.IsVariableName(name) {
		return errInvalidVariableName
	}

	var template database.Template
	err = h.db.InTx(c.Context(), func(q *database.Queries) error {
		released, err := q.RemoveOperatorRequiredVariable(c.Context(), database.RemoveOperatorRequiredVariableParams{ID: id, Name: name})
		if err != nil {
			return err
		}
		if slices.Contains(released.ContractRequiredVariables, name) {
			return errContractRequiredVariable.WithParams(map[string]interface{}{"name": name})
		}
		template = released
		return nil
	})
	if err != nil {
		return err
	}
	return c.JSON(template)
}
