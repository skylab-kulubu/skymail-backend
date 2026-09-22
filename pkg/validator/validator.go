package validator

import (
	"reflect"
	"regexp"
	"strings"

	"github.com/go-playground/validator/v10"
)

// A template key is the stable handle a service addresses a system template by
// (keycloak.verify-email, core.welcome). Lowercase so it is safe to compare and
// to put in a URL, bounded so it stays readable in the admin list.
//
// The same shape is enforced in the database by the templates_key_format check
// constraint. Keep the two in step: this one exists so a bad key is a 400 from
// the handler rather than a constraint violation surfacing as a 500.
var templateKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,62}[a-z0-9]$`)

// IsTemplateKey reports whether a key is well formed. Handlers that take a key
// from the path use it, since the struct tag only covers keys in a body.
func IsTemplateKey(key string) bool {
	return templateKeyPattern.MatchString(key)
}

// A Required variable is named as a body reaches it with .Name: letters,
// digits and underscores, not starting with a digit. The
// templates_required_variable_names check constraint holds the same shape;
// this one makes a bad name a 400 rather than a 500.
var variableNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// IsVariableName reports whether a Required variable's name is well formed.
func IsVariableName(name string) bool {
	return variableNamePattern.MatchString(name)
}

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

	_ = vld.RegisterValidation("templatekey", func(fl validator.FieldLevel) bool {
		return IsTemplateKey(fl.Field().String())
	})

	_ = vld.RegisterValidation("variablename", func(fl validator.FieldLevel) bool {
		return IsVariableName(fl.Field().String())
	})

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
	case "templatekey":
		return "invalid_template_key"
	case "variablename":
		return "invalid_variable_name"
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
