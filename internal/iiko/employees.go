package iiko

import (
	"context"
	"fmt"
	"time"

	"greenroom-ai/internal/analytics"
)

type Employee struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Attendance struct {
	ID, EmployeeID             string
	Start                      time.Time
	End                        *time.Time
	PersonalStart, PersonalEnd *time.Time
}

func (c *Client) Check(ctx context.Context) error { return c.checkScope(ctx, true) }

func (c *Client) checkScope(ctx context.Context, force bool) error {
	c.scopeMu.Lock()
	defer c.scopeMu.Unlock()
	if !force && time.Now().Before(c.scopeUntil) {
		return nil
	}
	var response struct {
		Organizations []struct {
			ID string `json:"id"`
		} `json:"organizations"`
	}
	if err := c.post(ctx, "/api/1/organizations", map[string]any{"returnAdditionalInfo": false, "includeDisabled": false}, &response); err != nil {
		return fmt.Errorf("check organizations: %w", err)
	}
	// OLAP request has no organizationId field. A multi-organization key could
	// silently expose another venue's sales; refuse until report scope is proven.
	if len(response.Organizations) != 1 || response.Organizations[0].ID != c.config.OrganizationID {
		return ErrOrganizationScope
	}
	c.scopeUntil = time.Now().Add(5 * time.Minute)
	return nil
}
func (c *Client) Employees(ctx context.Context) ([]Employee, error) {
	const limit = 100
	employees := make([]Employee, 0)
	seen := make(map[string]struct{})
	for offset := 0; offset < 10000; {
		var response struct {
			Items []struct {
				ID                 string   `json:"id"`
				Name               string   `json:"name"`
				MainOrganizationID string   `json:"mainOrganizationId"`
				OrganizationIDs    []string `json:"organizationIds"`
			} `json:"items"`
			TotalCount *int `json:"totalCount"`
		}
		req := map[string]any{"filters": []any{}, "fields": []string{"id", "name", "mainOrganizationId", "organizationIds"}, "limit": limit, "offset": offset}
		if err := c.post(ctx, "/api/employees/v1/employee/list", req, &response); err != nil {
			return nil, fmt.Errorf("list employees: %w", err)
		}
		newIdentities := 0
		for _, employee := range response.Items {
			if employee.ID == "" || employee.Name == "" {
				return nil, fmt.Errorf("employee identity: %w", ErrInvalidResponse)
			}
			if _, exists := seen[employee.ID]; exists {
				continue
			}
			seen[employee.ID] = struct{}{}
			newIdentities++
			if employee.MainOrganizationID == "" && employee.OrganizationIDs == nil {
				return nil, fmt.Errorf("missing employee organization membership: %w", ErrInvalidResponse)
			}
			belongs := employee.MainOrganizationID == c.config.OrganizationID
			for _, id := range employee.OrganizationIDs {
				if id == c.config.OrganizationID {
					belongs = true
					break
				}
			}
			if belongs {
				employees = append(employees, Employee{ID: employee.ID, Name: employee.Name})
			}
		}
		if response.Items == nil {
			return nil, fmt.Errorf("missing employees array: %w", ErrInvalidResponse)
		}
		if offset > 0 && len(response.Items) > 0 && newIdentities == 0 {
			return nil, fmt.Errorf("employees pagination made no progress: %w", ErrInvalidResponse)
		}
		done, err := paginationComplete(offset, len(response.Items), limit, response.TotalCount)
		if err != nil {
			return nil, err
		}
		offset += len(response.Items)
		if done {
			return employees, nil
		}
	}
	return nil, ErrResultLimit
}
func (c *Client) Attendances(ctx context.Context, employeeID string, p analytics.Period) ([]Attendance, error) {
	if employeeID == "" {
		return nil, ErrValidation
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	const limit = 100
	result := make([]Attendance, 0)
	seen := make(map[string]struct{})
	for offset := 0; offset < 10000; {
		var response struct {
			Items []struct {
				ID            string  `json:"id"`
				EmployeeID    string  `json:"employeeId"`
				Start         string  `json:"startAt"`
				End           *string `json:"endAt"`
				PersonalStart *string `json:"personalStartAt"`
				PersonalEnd   *string `json:"personalEndAt"`
			} `json:"items"`
			TotalCount *int `json:"totalCount"`
		}
		req := map[string]any{"organizationId": c.config.OrganizationID, "employeeIds": []string{employeeID}, "startAt": p.From.In(c.config.Location).Format(time.RFC3339), "endAt": p.To.In(c.config.Location).Format(time.RFC3339), "shouldIncludePaymentDetails": false, "limit": limit, "offset": offset}
		if err := c.post(ctx, "/api/employees/v1/attendance/list", req, &response); err != nil {
			return nil, fmt.Errorf("list attendances: %w", err)
		}
		newIdentities := 0
		for _, item := range response.Items {
			if item.ID == "" || item.EmployeeID != employeeID {
				return nil, fmt.Errorf("attendance identity: %w", ErrInvalidResponse)
			}
			if _, exists := seen[item.ID]; exists {
				continue
			}
			seen[item.ID] = struct{}{}
			newIdentities++
			start, err := parseAttendanceTime(item.Start)
			if err != nil {
				return nil, err
			}
			end, err := parseOptionalTime(item.End)
			if err != nil {
				return nil, err
			}
			personalStart, err := parseOptionalTime(item.PersonalStart)
			if err != nil {
				return nil, err
			}
			personalEnd, err := parseOptionalTime(item.PersonalEnd)
			if err != nil {
				return nil, err
			}
			if end != nil && end.Before(start) {
				return nil, fmt.Errorf("attendance end precedes start: %w", ErrInvalidResponse)
			}
			result = append(result, Attendance{ID: item.ID, EmployeeID: item.EmployeeID, Start: start, End: end, PersonalStart: personalStart, PersonalEnd: personalEnd})
		}
		if response.Items == nil {
			return nil, fmt.Errorf("missing attendances array: %w", ErrInvalidResponse)
		}
		if offset > 0 && len(response.Items) > 0 && newIdentities == 0 {
			return nil, fmt.Errorf("attendances pagination made no progress: %w", ErrInvalidResponse)
		}
		done, err := paginationComplete(offset, len(response.Items), limit, response.TotalCount)
		if err != nil {
			return nil, err
		}
		offset += len(response.Items)
		if done {
			return result, nil
		}
	}
	return nil, ErrResultLimit
}
func parseAttendanceTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("attendance timestamp requires explicit offset: %w", ErrInvalidResponse)
	}
	return parsed, nil
}
func parseOptionalTime(value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := parseAttendanceTime(*value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func paginationComplete(offset, size, limit int, total *int) (bool, error) {
	if total != nil {
		if *total < 0 || offset+size > *total || (size == 0 && offset < *total) {
			return false, fmt.Errorf("inconsistent paginated result: %w", ErrInvalidResponse)
		}
		return offset+size == *total, nil
	}
	return size < limit, nil
}
