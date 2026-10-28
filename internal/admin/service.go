package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"travel-app-backend/internal/bookings"
	"travel-app-backend/internal/pkg/pagination"

	"github.com/jackc/pgx/v5"
)

// Domain errors.
var (
	ErrNotFound          = errors.New("not found")
	ErrInvalidTransition = errors.New("invalid booking status transition")
	ErrInvalidDates      = errors.New("check_out must be after check_in")
	ErrInvalidStatus     = errors.New("invalid booking status")
	ErrInvalidRole       = errors.New("role must be 'user' or 'admin'")
	ErrHotelHasBookings  = errors.New("hotel still has bookings and cannot be deleted")
	ErrRoomHasBookings   = errors.New("room still has bookings and cannot be deleted — deactivate it instead")
)

// statusTransitions is the admin side of the booking status machine:
// pending → confirmed → complete, with cancel from pending/confirmed.
// complete and cancelled are terminal. Matches the Phase 0 CHECK and the
// user-facing endpoints (cancel/reschedule stay pending-only).
var statusTransitions = map[string][]string{
	bookings.StatusPending:   {bookings.StatusConfirmed, bookings.StatusCancelled},
	bookings.StatusConfirmed: {bookings.StatusComplete, bookings.StatusCancelled},
}

// validStatuses is every status an admin filter/edit may name.
var validStatuses = map[string]bool{
	bookings.StatusPending:   true,
	bookings.StatusConfirmed: true,
	bookings.StatusComplete:  true,
	bookings.StatusCancelled: true,
}

// validRoles for PATCH /admin/users/:id/role.
var validRoles = map[string]bool{"user": true, "admin": true}

// Service holds admin business logic behind the Repository.
type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// ── Bookings ──────────────────────────────────────────────────────────

// ListBookings returns a filtered, paginated booking list.
func (s *Service) ListBookings(ctx context.Context, f BookingFilter) ([]AdminBooking, int64, error) {
	if f.Status != "" && !validStatuses[f.Status] {
		return nil, 0, fmt.Errorf("%w: %q", ErrInvalidStatus, f.Status)
	}
	for label, v := range map[string]string{"check_in from": f.From, "check_in to": f.To} {
		if v != "" {
			if _, err := time.Parse("2006-01-02", v); err != nil {
				return nil, 0, fmt.Errorf("%s must be YYYY-MM-DD", label)
			}
		}
	}
	return s.repo.ListBookings(ctx, f)
}

// GetBooking returns one booking.
func (s *Service) GetBooking(ctx context.Context, id int64) (*AdminBooking, error) {
	b, err := s.repo.GetBooking(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// ChangeStatus transitions a booking through the status machine and
// writes an audit entry (reason lives in the audit payload only).
func (s *Service) ChangeStatus(ctx context.Context, adminID, id int64, req UpdateStatusRequest) (*AdminBooking, error) {
	if !validStatuses[req.Status] {
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, req.Status)
	}
	current, err := s.GetBooking(ctx, id)
	if err != nil {
		return nil, err
	}
	allowed, ok := statusTransitions[current.Status]
	if !ok || current.Status == req.Status {
		return nil, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, req.Status)
	}
	// allowed must contain the target (terminal states have no entry).
	targetAllowed := false
	for _, a := range allowed {
		if a == req.Status {
			targetAllowed = true
			break
		}
	}
	if !targetAllowed {
		return nil, fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, req.Status)
	}

	if err := s.repo.UpdateBookingStatus(ctx, id, req.Status, current.Status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Concurrent edit raced the transition — re-read for a clear error.
			if _, gerr := s.GetBooking(ctx, id); gerr != nil {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("%w: status changed concurrently", ErrInvalidTransition)
		}
		return nil, err
	}

	_ = s.repo.InsertAudit(ctx, adminID, "booking.status_changed", "booking", id, map[string]any{
		"from":   current.Status,
		"to":     req.Status,
		"reason": strings.TrimSpace(req.Reason),
	})
	return s.GetBooking(ctx, id)
}

// EditBooking patches guest fields/dates on an active booking. Dates
// re-fire the no_double_booking constraint → ErrNotAvailable → 409.
func (s *Service) EditBooking(ctx context.Context, adminID, id int64, req UpdateBookingRequest) (*AdminBooking, error) {
	current, err := s.GetBooking(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.Status != bookings.StatusPending && current.Status != bookings.StatusConfirmed {
		return nil, fmt.Errorf("%w: finished bookings cannot be edited", ErrInvalidTransition)
	}
	// Validate the merged date pair before touching the row.
	if req.CheckIn != nil || req.CheckOut != nil {
		in, out := current.CheckIn, current.CheckOut
		if req.CheckIn != nil {
			in = *req.CheckIn
		}
		if req.CheckOut != nil {
			out = *req.CheckOut
		}
		tIn, err1 := time.Parse("2006-01-02", in)
		tOut, err2 := time.Parse("2006-01-02", out)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("%w: dates must be YYYY-MM-DD", ErrInvalidDates)
		}
		if !tOut.After(tIn) {
			return nil, ErrInvalidDates
		}
	}

	if err := s.repo.UpdateBooking(ctx, id, req); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if IsExclusionViolation(err) {
			return nil, bookings.ErrNotAvailable
		}
		return nil, err
	}

	updated, err := s.GetBooking(ctx, id)
	if err != nil {
		return nil, err
	}
	_ = s.repo.InsertAudit(ctx, adminID, "booking.updated", "booking", id, map[string]any{
		"before": map[string]any{
			"guest_name": current.GuestName, "guest_phone": current.GuestPhone,
			"check_in": current.CheckIn, "check_out": current.CheckOut, "guests": current.Guests,
		},
		"after": map[string]any{
			"guest_name": updated.GuestName, "guest_phone": updated.GuestPhone,
			"check_in": updated.CheckIn, "check_out": updated.CheckOut, "guests": updated.Guests,
		},
	})
	return updated, nil
}

// ── Hotels & rooms ────────────────────────────────────────────────────

// ListHotels returns hotels with counts.
func (s *Service) ListHotels(ctx context.Context, q string, page pagination.Page) ([]AdminHotel, int64, error) {
	return s.repo.ListHotels(ctx, q, page)
}

// GetHotel returns one hotel with its rooms.
func (s *Service) GetHotel(ctx context.Context, id int64) (*AdminHotel, error) {
	h, err := s.repo.GetHotel(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return h, err
}

// CreateHotel inserts a hotel and audits it.
func (s *Service) CreateHotel(ctx context.Context, adminID int64, req CreateHotelRequest) (*AdminHotel, error) {
	h, err := s.repo.CreateHotel(ctx, req)
	if err != nil {
		return nil, err
	}
	_ = s.repo.InsertAudit(ctx, adminID, "hotel.created", "hotel", h.ID, map[string]any{
		"name": h.Name, "place": h.Place,
	})
	return h, nil
}

// UpdateHotel patches a hotel and audits it.
func (s *Service) UpdateHotel(ctx context.Context, adminID, id int64, req UpdateHotelRequest) (*AdminHotel, error) {
	before, err := s.GetHotel(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.repo.UpdateHotel(ctx, id, req); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	after, err := s.GetHotel(ctx, id)
	if err != nil {
		return nil, err
	}
	_ = s.repo.InsertAudit(ctx, adminID, "hotel.updated", "hotel", id, map[string]any{
		"before": map[string]any{"name": before.Name, "place": before.Place, "rating": before.Rating},
		"after":  map[string]any{"name": after.Name, "place": after.Place, "rating": after.Rating},
	})
	return after, nil
}

// DeleteHotel refuses when bookings reference it (bookings.hotel_id is
// ON DELETE CASCADE — a raw DELETE would silently destroy history).
func (s *Service) DeleteHotel(ctx context.Context, adminID, id int64) error {
	h, err := s.GetHotel(ctx, id)
	if err != nil {
		return err
	}
	n, err := s.repo.CountHotelBookings(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %d booking(s) — cancel them first or deactivate the hotel", ErrHotelHasBookings, n)
	}
	if err := s.repo.DeleteHotel(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return s.repo.InsertAudit(ctx, adminID, "hotel.deleted", "hotel", id, map[string]any{
		"name": h.Name, "place": h.Place,
	})
}

// CreateRoom adds a room type to a hotel and audits it.
func (s *Service) CreateRoom(ctx context.Context, adminID, hotelID int64, req RoomRequest) (*AdminRoom, error) {
	if _, err := s.GetHotel(ctx, hotelID); err != nil {
		return nil, err
	}
	rm, err := s.repo.CreateRoom(ctx, hotelID, req)
	if err != nil {
		return nil, err
	}
	_ = s.repo.InsertAudit(ctx, adminID, "room.created", "room", rm.ID, map[string]any{
		"hotel_id": hotelID, "name": rm.Name, "price_mmk": rm.PriceMMK, "total_units": rm.TotalUnits,
	})
	return rm, nil
}

// UpdateRoom patches a room and audits it.
func (s *Service) UpdateRoom(ctx context.Context, adminID, hotelID, roomID int64, req RoomRequest) (*AdminRoom, error) {
	before, err := s.repo.GetRoom(ctx, hotelID, roomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.repo.UpdateRoom(ctx, hotelID, roomID, req); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	after, err := s.repo.GetRoom(ctx, hotelID, roomID)
	if err != nil {
		return nil, err
	}
	_ = s.repo.InsertAudit(ctx, adminID, "room.updated", "room", roomID, map[string]any{
		"hotel_id": hotelID,
		"before":   map[string]any{"name": before.Name, "price_mmk": before.PriceMMK, "is_active": before.IsActive, "total_units": before.TotalUnits},
		"after":    map[string]any{"name": after.Name, "price_mmk": after.PriceMMK, "is_active": after.IsActive, "total_units": after.TotalUnits},
	})
	return after, nil
}

// DeleteRoom maps the bookings.room_id RESTRICT FK (23503) to a 409:
// history-bearing rooms can only be deactivated, never deleted.
func (s *Service) DeleteRoom(ctx context.Context, adminID, hotelID, roomID int64) error {
	rm, err := s.repo.GetRoom(ctx, hotelID, roomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.repo.DeleteRoom(ctx, hotelID, roomID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if isFKViolation(err) {
			return fmt.Errorf("%w: room %q", ErrRoomHasBookings, rm.Name)
		}
		return err
	}
	return s.repo.InsertAudit(ctx, adminID, "room.deleted", "room", roomID, map[string]any{
		"hotel_id": hotelID, "name": rm.Name,
	})
}

// SetRoomInventory updates allotment + stop-sell and audits it.
func (s *Service) SetRoomInventory(ctx context.Context, adminID, hotelID, roomID int64, req InventoryRequest) (*AdminRoom, error) {
	before, err := s.repo.GetRoom(ctx, hotelID, roomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.repo.SetRoomInventory(ctx, hotelID, roomID, req.TotalUnits, req.IsActive); err != nil {
		return nil, err
	}
	after, err := s.repo.GetRoom(ctx, hotelID, roomID)
	if err != nil {
		return nil, err
	}
	_ = s.repo.InsertAudit(ctx, adminID, "room.inventory_changed", "room", roomID, map[string]any{
		"hotel_id": hotelID,
		"before":   map[string]any{"total_units": before.TotalUnits, "is_active": before.IsActive},
		"after":    map[string]any{"total_units": after.TotalUnits, "is_active": after.IsActive},
	})
	return after, nil
}

// ── Users ─────────────────────────────────────────────────────────────

// ListUsers returns users with booking counts.
func (s *Service) ListUsers(ctx context.Context, q, role string, page pagination.Page) ([]AdminUser, int64, error) {
	if role != "" && !validRoles[role] {
		return nil, 0, fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
	return s.repo.ListUsers(ctx, q, role, page)
}

// SetUserRole flips users.role and audits it. Note: the Firebase custom
// claim is NOT touched here — DB role is the revocable source of truth;
// a demoted admin loses access on their next request (claim fast path
// only grants, never overrides a DB demotion, per AdminRequired).
func (s *Service) SetUserRole(ctx context.Context, adminID, id int64, req SetRoleRequest) (*AdminUser, error) {
	role := strings.TrimSpace(strings.ToLower(req.Role))
	if !validRoles[role] {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRole, req.Role)
	}
	before, err := s.repo.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.repo.SetUserRole(ctx, id, role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	after, err := s.repo.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	_ = s.repo.InsertAudit(ctx, adminID, "user.role_changed", "user", id, map[string]any{
		"email":  after.Email,
		"before": before.Role,
		"after":  after.Role,
	})
	return after, nil
}

// ── Stats & audit ─────────────────────────────────────────────────────

// StatsOverview returns the dashboard aggregate.
func (s *Service) StatsOverview(ctx context.Context) (*OverviewStats, error) {
	return s.repo.StatsOverview(ctx)
}

// Occupancy returns per-night occupancy for [from, to).
func (s *Service) Occupancy(ctx context.Context, from, to string, hotelID int64) ([]OccupancyDay, error) {
	tFrom, err1 := time.Parse("2006-01-02", from)
	tTo, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("from/to must be YYYY-MM-DD")
	}
	if !tTo.After(tFrom) {
		return nil, fmt.Errorf("to must be after from")
	}
	if tTo.Sub(tFrom) > 366*24*time.Hour {
		return nil, fmt.Errorf("range too large (max 366 days)")
	}
	return s.repo.Occupancy(ctx, from, to, hotelID)
}

// ListAuditLogs returns the admin action trail.
func (s *Service) ListAuditLogs(ctx context.Context, f AuditFilter) ([]AuditLog, int64, error) {
	return s.repo.ListAuditLogs(ctx, f)
}
