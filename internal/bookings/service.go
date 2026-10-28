package bookings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Domain errors. ErrNotAvailable is raised when no unit of the room is
// free for the window (availability engine or the unit-scoped
// no_double_booking EXCLUDE constraint firing). ErrRoomInactive is the
// admin stop-sell switch (hotel_rooms.is_active = false).
var (
	ErrNotFound           = errors.New("booking not found")
	ErrRoomNotFound       = errors.New("room not found for hotel")
	ErrInvalidDates       = errors.New("check_out must be after check_in")
	ErrNotPending         = errors.New("booking can no longer be changed")
	ErrForbidden          = errors.New("you do not own this booking")
	ErrNotAvailable       = errors.New("room is not available for the selected dates")
	ErrRoomInactive       = errors.New("room is not currently bookable")
	ErrHotelNotFound      = errors.New("hotel not found")
	ErrPaymentUnsupported = errors.New("this payment method is not supported yet")
)

// Service holds booking business logic behind the Repository interface.
type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create books a room for the authenticated user. Under pay-at-property
// the booking is BORN CONFIRMED: no online money moves, so there is no
// tentative-hold stage — the guest settles at the front desk. The unit
// is still assigned inside the same serialized transaction, and the
// unit-scoped exclusion constraint is still the hard backstop.
func (s *Service) Create(ctx context.Context, userID int64, req CreateRequest) (*Booking, error) {
	checkIn, checkOut, nights, err := parseDates(req.CheckIn, req.CheckOut)
	if err != nil {
		return nil, err
	}

	// Only pay-at-property is supported until a gateway lands. Omitted
	// means pay-at-property (the mobile app and older clients omit it).
	paymentMethod := strings.TrimSpace(req.PaymentMethod)
	if paymentMethod == "" {
		paymentMethod = PaymentMethodPayAtProperty
	}
	if paymentMethod != PaymentMethodPayAtProperty {
		return nil, ErrPaymentUnsupported
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

	now := time.Now().UTC()
	b := &Booking{
		UserID:        userID,
		HotelID:       req.HotelID,
		RoomID:        req.RoomID,
		HotelName:     pricing.HotelName,
		RoomName:      pricing.RoomName,
		Place:         pricing.Place,
		ImageURL:      pricing.ImageURL,
		PriceMMK:      pricing.PriceMMK,
		Nights:        nights,
		TotalMMK:      pricing.PriceMMK * int64(nights),
		GuestName:     strings.TrimSpace(req.GuestName),
		GuestPhone:    strings.TrimSpace(req.GuestPhone),
		CheckIn:       checkIn,
		CheckOut:      checkOut,
		Guests:        guests,
		Status:        StatusConfirmed,
		PaymentMethod: paymentMethod,
		ConfirmedAt:   &now,
	}
	if err := s.repo.Create(ctx, b); err != nil {
		if IsExclusionViolation(err) {
			return nil, ErrNotAvailable
		}
		if errors.Is(err, ErrNotAvailable) || errors.Is(err, ErrRoomInactive) {
			return nil, err
		}
		return nil, fmt.Errorf("create booking: %w", err)
	}
	return b, nil
}

// Availability returns the real-time bookable state of every room of a
// hotel for a date window. Public endpoint — the client uses it to grey
// out sold-out/stop-sold rooms BEFORE the guest taps Book.
func (s *Service) Availability(ctx context.Context, hotelID int64, checkIn, checkOut string) (*AvailabilityResponse, error) {
	in, out, nights, err := parseDates(checkIn, checkOut)
	if err != nil {
		return nil, err
	}
	exists, err := s.repo.HotelExists(ctx, hotelID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrHotelNotFound
	}
	rooms, err := s.repo.RoomAvailability(ctx, hotelID, in, out)
	if err != nil {
		return nil, err
	}
	if rooms == nil {
		rooms = []RoomAvailability{}
	}
	return &AvailabilityResponse{
		HotelID:  hotelID,
		CheckIn:  in,
		CheckOut: out,
		Nights:   nights,
		Rooms:    rooms,
	}, nil
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
	case "", StatusPending, StatusConfirmed, StatusComplete, StatusCancelled:
	default:
		return nil, fmt.Errorf("invalid status %q", status)
	}
	// The mobile app has only two tabs — "Pending" (upcoming stays) and
	// "Complete". Under pay-at-property new bookings are born 'confirmed',
	// so the 'pending' filter carries UPCOMING semantics (pending +
	// confirmed) server-side rather than making every client remember to
	// union the two statuses. Explicit 'confirmed'/'complete'/'cancelled'
	// still filter exactly.
	if status == StatusPending {
		return s.repo.ListByUser(ctx, userID, StatusPending, StatusConfirmed)
	}
	return s.repo.ListByUser(ctx, userID, status)
}

// Cancel cancels an upcoming booking (owner only). Both pending and
// confirmed bookings are cancellable: under pay-at-property no online
// money has moved, so there is nothing to refund — the front desk simply
// never charges the no-show.
func (s *Service) Cancel(ctx context.Context, userID, id int64) (*Booking, error) {
	b, err := s.ownedBooking(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if b.Status != StatusPending && b.Status != StatusConfirmed {
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
	in, out, _, err := parseDates(checkIn, checkOut)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Reschedule(ctx, id, in, out); err != nil {
		if IsExclusionViolation(err) {
			return nil, ErrNotAvailable
		}
		if errors.Is(err, ErrNotAvailable) || errors.Is(err, ErrRoomInactive) {
			return nil, err
		}
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

// parseDates validates YYYY-MM-DD bounds and returns the stay length in
// nights (checkout-exclusive: Jan 1 → Jan 3 is 2 nights — the window the
// no_double_booking exclusion constraint occupies).
func parseDates(in, out string) (string, string, int, error) {
	checkIn, err := time.Parse("2006-01-02", in)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid check_in, use YYYY-MM-DD")
	}
	checkOut, err := time.Parse("2006-01-02", out)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid check_out, use YYYY-MM-DD")
	}
	if !checkOut.After(checkIn) {
		return "", "", 0, ErrInvalidDates
	}
	return in, out, int(checkOut.Sub(checkIn).Hours() / 24), nil
}
