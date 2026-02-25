package requests

type CreateMailingList struct {
	Name string `json:"name" validate:"required"`
}

type UpdateMailingList struct {
	Name string `json:"name" validate:"required"`
}

type AddRecipient struct {
	FullName string `json:"full_name" validate:"required"`
	Email    string `json:"email" validate:"required,email"`
}
