package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type User struct {
	ID                  uuid.UUID  `json:"id"`
	GitHubID            int64      `json:"github_id"`
	Email               string     `json:"email"`
	RazorpayCustomerID  *string    `json:"razorpay_customer_id"`
	PlanTier            string     `json:"plan_tier"`
	PlanPeriod          string     `json:"plan_period"`
	PlanPaidAt          *time.Time `json:"plan_paid_at"`
	CreatedAt           time.Time  `json:"created_at"`
}

type Claims struct {
	UserID uuid.UUID `json:"user_id"`
	jwt.RegisteredClaims
}

func (s *server) generateJWT(userID uuid.UUID) (string, error) {
	claims := Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.cfg.JWT.Secret))
}

func (s *server) parseJWT(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		return []byte(s.cfg.JWT.Secret), nil
	})
	if err != nil {
		return nil, err
	}
	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}
	return nil, fmt.Errorf("invalid token")
}

func (s *server) getUserFromRequest(r *http.Request) (*User, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil, fmt.Errorf("no session cookie")
	}
	claims, err := s.parseJWT(cookie.Value)
	if err != nil {
		return nil, err
	}
	var user User
	err = s.db.QueryRow(
		`SELECT id, github_id, email, razorpay_customer_id, plan_tier, plan_period, plan_paid_at, created_at
		 FROM users WHERE id = $1`, claims.UserID,
	).Scan(&user.ID, &user.GitHubID, &user.Email, &user.RazorpayCustomerID,
		&user.PlanTier, &user.PlanPeriod, &user.PlanPaidAt, &user.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *server) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	state := make([]byte, 16)
	rand.Read(state)
	stateStr := fmt.Sprintf("%x", state)

	// Store state in session or redis for verification
	// For simplicity, using cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    stateStr,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   300, // 5 min
	})

	githubURL := fmt.Sprintf("https://github.com/login/oauth/authorize?client_id=%s&redirect_uri=%s&scope=user:email&state=%s",
		s.cfg.GitHub.ClientID, url.QueryEscape(s.cfg.GitHub.RedirectURI), stateStr)
	http.Redirect(w, r, githubURL, http.StatusFound)
}

func (s *server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	// Verify state
	cookie, err := r.Cookie("oauth_state")
	if err != nil || cookie.Value != state {
		http.Error(w, "Invalid state", http.StatusBadRequest)
		return
	}

	// Exchange code for token
	data := url.Values{}
	data.Set("client_id", s.cfg.GitHub.ClientID)
	data.Set("client_secret", s.cfg.GitHub.ClientSecret)
	data.Set("code", code)
	data.Set("redirect_uri", s.cfg.GitHub.RedirectURI)

	resp, err := http.PostForm("https://github.com/login/oauth/access_token", data)
	if err != nil {
		http.Error(w, "Failed to get access token", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read response", http.StatusInternalServerError)
		return
	}

	values, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "Failed to parse response", http.StatusInternalServerError)
		return
	}

	accessToken := values.Get("access_token")
	if accessToken == "" {
		http.Error(w, "No access token", http.StatusInternalServerError)
		return
	}

	// Get user info
	req, _ := http.NewRequest("GET", "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{}
	userResp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Failed to get user info", http.StatusInternalServerError)
		return
	}
	defer userResp.Body.Close()

	var githubUser struct {
		ID    int64  `json:"id"`
		Email string `json:"email"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(userResp.Body).Decode(&githubUser); err != nil {
		http.Error(w, "Failed to decode user info", http.StatusInternalServerError)
		return
	}

	// If email is null, get from emails endpoint
	if githubUser.Email == "" {
		req, _ := http.NewRequest("GET", "https://api.github.com/user/emails", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		emailsResp, err := client.Do(req)
		if err == nil {
			defer emailsResp.Body.Close()
			var emails []struct {
				Email    string `json:"email"`
				Primary  bool   `json:"primary"`
				Verified bool   `json:"verified"`
			}
			json.NewDecoder(emailsResp.Body).Decode(&emails)
			for _, e := range emails {
				if e.Primary && e.Verified {
					githubUser.Email = e.Email
					break
				}
			}
		}
	}

	// Upsert user
	var userID uuid.UUID
	err = s.db.QueryRow(`
		INSERT INTO users (github_id, email) VALUES ($1, $2)
		ON CONFLICT (github_id) DO UPDATE SET email = EXCLUDED.email
		RETURNING id`, githubUser.ID, githubUser.Email).Scan(&userID)
	if err != nil {
		http.Error(w, "Failed to create user", http.StatusInternalServerError)
		return
	}

	// Generate JWT
	token, err := s.generateJWT(userID)
	if err != nil {
		http.Error(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}

	// Session cookie. Shared across api.instanode.dev and instanode.dev so
	// the static marketing/dashboard pages can authenticate fetch() calls
	// against the API. SameSite=None + Secure are required by modern
	// browsers for cross-site cookie sends.
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		Domain:   "instanode.dev",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
		MaxAge:   86400, // 24 hours
	})

	// After login, drop the user on the dashboard on the marketing domain.
	http.Redirect(w, r, "https://instanode.dev/dashboard.html", http.StatusFound)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		Domain:   "instanode.dev",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "https://instanode.dev/", http.StatusFound)
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.getUserFromRequest(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	json.NewEncoder(w).Encode(user)
}