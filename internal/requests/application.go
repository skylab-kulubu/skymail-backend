package requests

type CreateApplication struct {
	Name string `json:"name" validate:"required"`
}

type UpdateApplication struct {
	Name string `json:"name" validate:"required"`
}
