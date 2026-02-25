package validator

import (
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
)

type StructValidator interface {
	Validate(out any) error
}

type structValidator struct {
	validate *validator.Validate
}

type FieldError struct {
	Field  string                 `json:"field"`
	Code   string                 `json:"code"`
	Params map[string]interface{} `json:"params,omitempty"`
}

type ValidationErrors = validator.ValidationErrors

func NewStructValidator() StructValidator {
	vld := validator.New()

	vld.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})

	return &structValidator{
		validate: vld,
	}
}

func (v *structValidator) Validate(out any) error {
	return v.validate.Struct(out)
}

func ParseValidationErrors(validationErrors validator.ValidationErrors) []FieldError {
	errs := make([]FieldError, len(validationErrors))
	for i, ve := range validationErrors {
		errs[i] = FieldError{
			Field:  ve.Field(),
			Code:   getErrorCode(ve),
			Params: getErrorParams(ve),
		}
	}

	return errs
}

func getErrorCode(e validator.FieldError) string {
	switch e.Tag() {
	case "required":
		return "required"
	case "email":
		return "invalid_email"
	case "min":
		return "min_length"
	case "max":
		return "max_length"
	default:
		return "invalid"
	}
}

func getErrorParams(e validator.FieldError) map[string]interface{} {
	switch e.Tag() {
	case "min", "max":
		return map[string]interface{}{
			"limit": e.Param(),
		}
	default:
		return nil
	}
}
