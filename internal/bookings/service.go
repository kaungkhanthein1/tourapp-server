package bookings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a booking does not exist.
var (
	ErrNotFound     = errors.New("booking not found")
	ErrRoomNotFound = errors.New("room not found for hotel")
	ErrInvalidDates = errors.New("check_out must be after check_in")
	ErrNotPending   = errors.New("booking is not pending")
	ErrForbidden    = errors.New("you do not own this booking")
)

// Service holds booking business logic behind the Repository interface.
type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create books a room for the authenticated user.
func (s *Service) Create(ctx context.Context, userID int64, req CreateRequest) (*Booking, error) {
	checkIn, checkOut, err := parseDates(req.CheckIn, req.CheckOut)
	if err != nil {
		return nil, err
	}

	pricing, err := s.repo.RoomPricing(ctx, req.HotelID, req.RoomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoomNotFound
		}
		return nil, fmt.Errorf("room pricing: %w", err)
	}

	guests := req.Guests
	if guests <= 0 {
		guests = 1
	}

	b := &Booking{
		UserID:    userID,
		HotelID:   req.HotelID,
		HotelName: pricing.HotelName,
		RoomName:  pricing.RoomName,
		Place:     pricing.Place,
		ImageURL:  pricing.ImageURL,
		PriceMMK:  pricing.PriceMMK,
		CheckIn:   checkIn,
		CheckOut:  checkOut,
		Guests:    guests,
		Status:    StatusPending,
	}
	if err := s.repo.Create(ctx, b); err != nil {
		return nil, fmt.Errorf("create booking: %w", err)
	}
	return b, nil
}

func (s *Service) GetByID(ctx context.Context, id int64) (*Booking, error) {
	b, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return b, nil
}

func (s *Service) ListByUser(ctx context.Context, userID int64, status string) ([]Booking, error) {
	switch status {
	case "", StatusPending, StatusComplete, StatusCancelled:
	default:
		return nil, fmt.Errorf("invalid status %q", status)
	}
	return s.repo.ListByUser(ctx, userID, status)
}

// Cancel sets a pending booking to cancelled (owner only).
func (s *Service) Cancel(ctx context.Context, userID, id int64) (*Booking, error) {
	b, err := s.ownedBooking(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusPending {
		return nil, ErrNotPending
	}
	if err := s.repo.UpdateStatus(ctx, id, StatusCancelled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	b.Status = StatusCancelled
	return b, nil
}

// Reschedule updates dates of a pending booking (owner only).
func (s *Service) Reschedule(ctx context.Context, userID, id int64, checkIn, checkOut string) (*Booking, error) {
	if _, err := s.ownedBooking(ctx, userID, id); err != nil {
		return nil, err
	}
	in, out, err := parseDates(checkIn, checkOut)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Reschedule(ctx, id, in, out); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := s.GetByID(ctx, id); gerr != nil {
				return nil, ErrNotFound
			}
			return nil, ErrNotPending
		}
		return nil, err
	}
	return s.GetByID(ctx, id)
}

// ownedBooking fetches a booking and verifies ownership.
func (s *Service) ownedBooking(ctx context.Context, userID, id int64) (*Booking, error) {
	b, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.UserID != userID {
		return nil, ErrForbidden
	}
	return b, nil
}

func parseDates(in, out string) (string, string, error) {
	checkIn, err := time.Parse("2006-01-02", in)
	if err != nil {
		return "", "", fmt.Errorf("invalid check_in, use YYYY-MM-DD")
	}
	checkOut, err := time.Parse("2006-01-02", out)
	if err != nil {
		return "", "", fmt.Errorf("invalid check_out, use YYYY-MM-DD")
	}
	if !checkOut.After(checkIn) {
		return "", "", ErrInvalidDates
	}
	return in, out, nil
}
