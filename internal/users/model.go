package users

import "time"

// User is the app account shown on the Profile screen.
// PasswordHash is never serialized to clients.
type User struct {
	ID           int64     `json:"id"`
	FirebaseUID  string    `json:"firebase_uid,omitempty"`
	Name         string    `json:"name"`
	Email        string    `json:"email"`
	Role         string    `json:"role"` // 'user' (default) | 'admin' — RBAC for /admin routes
	Phone        string    `json:"phone,omitempty"`
	AvatarURL    string    `json:"avatar_url,omitempty"`
	Bio          string    `json:"bio,omitempty"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
}

// RegisterRequest is the POST /users/register body.
type RegisterRequest struct {
	Name     string `json:"name" binding:"required"`
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
	Phone    string `json:"phone"`
}

// LoginRequest is the POST /users/login body.
type LoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// AuthResponse is returned by register/login.
type AuthResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

// UpdateRequest is the PUT /users/:id body (all fields optional).
type UpdateRequest struct {
	Name      *string `json:"name"`
	Phone     *string `json:"phone"`
	AvatarURL *string `json:"avatar_url"`
	Bio       *string `json:"bio"`
}
