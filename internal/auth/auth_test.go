package auth

import (
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func store(t *testing.T) (*Store, *clock) {
	c := &clock{time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)}
	s := NewStore(t.TempDir())
	s.Now = c.now
	return s, c
}

func TestSetupCodeExpiresWorksOnceAndOnlyBeforeTheFirstAdmin(t *testing.T) {
	s, c := store(t)
	code, err := s.SetupCode(30 * time.Minute)
	if err != nil || len(code) != 11 || code[5] != '-' {
		t.Fatalf("%q %v", code, err)
	}
	if strings.ContainsAny(code, "01OIL") {
		t.Fatal("no ambiguous characters")
	}
	c.t = c.t.Add(31 * time.Minute)
	if _, err := s.UseSetupCode(code, "ops"); err == nil {
		t.Fatal("an expired code is refused")
	}
	code, _ = s.SetupCode(30 * time.Minute)
	if _, err := s.UseSetupCode("WRONG-CODE0", "ops"); err == nil {
		t.Fatal("a wrong code is refused")
	}
	a, err := s.UseSetupCode(strings.ToLower(code), "ops") // typed loosely
	if err != nil || a.Name != "ops" {
		t.Fatalf("%v", err)
	}
	if _, err := s.UseSetupCode(code, "ops2"); err == nil {
		t.Fatal("a code works once")
	}
	// The first admin registers a credential: from then on there is no setup code at all.
	secret, _ := s.BeginTOTP("ops")
	if err := s.ConfirmTOTP("ops", TOTPAt(secret, uint64(c.t.Unix()/30))); err != nil {
		t.Fatal(err)
	}
	if s.NeedsSetup() {
		t.Fatal("an admin exists")
	}
	if _, err := s.SetupCode(time.Hour); err != ErrAdminsExist {
		t.Fatalf("no setup code once an admin exists: %v", err)
	}
}

func TestTOTPMatchesRFC6238AndAcceptsACodeOnce(t *testing.T) {
	// RFC 6238, appendix B: the SHA-1 key "12345678901234567890" at T=59 gives 94287082 (8 digits).
	if got := TOTPAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", 1); got != "287082" {
		t.Fatalf("%s", got)
	}
	s, c := store(t)
	code, _ := s.SetupCode(time.Hour)
	s.UseSetupCode(code, "ops")
	secret, _ := s.BeginTOTP("ops")
	if err := s.ConfirmTOTP("ops", "000000"); err == nil {
		t.Fatal("a wrong confirmation is refused")
	}
	step := uint64(c.t.Unix() / 30)
	s.ConfirmTOTP("ops", TOTPAt(secret, step))
	c.t = c.t.Add(30 * time.Second)
	cur := TOTPAt(secret, step+1)
	if _, err := s.CheckTOTP("ops", cur); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckTOTP("ops", cur); err == nil {
		t.Fatal("the same code twice is refused")
	}
	if _, err := s.CheckTOTP("ops", TOTPAt(secret, step)); err == nil {
		t.Fatal("an older code is refused")
	}
}

func TestAdminsAddResetRemove(t *testing.T) {
	s, c := store(t)
	code, _ := s.SetupCode(time.Hour)
	s.UseSetupCode(code, "first")
	sec, _ := s.BeginTOTP("first")
	s.ConfirmTOTP("first", TOTPAt(sec, uint64(c.t.Unix()/30)))

	enrol, err := s.Add("second", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("second", time.Hour); err == nil {
		t.Fatal("a name once")
	}
	if _, err := s.UseEnrolCode("second", "NOPE-NOPE"); err == nil {
		t.Fatal("wrong enrolment code")
	}
	if _, err := s.UseEnrolCode("second", enrol); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("first"); err == nil {
		t.Fatal("the last admin who can sign in stays (second has no credential yet)")
	}
	sec2, _ := s.BeginTOTP("second")
	s.ConfirmTOTP("second", TOTPAt(sec2, uint64(c.t.Unix()/30)))
	if err := s.Remove("first"); err != nil {
		t.Fatal(err)
	}
	newCode, err := s.Reset("second", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Find("second"); a.HasCredential() || a.Enroll == nil {
		t.Fatalf("reset drops the credentials: %+v", a)
	}
	c.t = c.t.Add(2 * time.Hour)
	if _, err := s.UseEnrolCode("second", newCode); err == nil {
		t.Fatal("enrolment codes expire")
	}
}

func TestSessionsAndLimits(t *testing.T) {
	c := &clock{time.Now()}
	ss := NewSessions()
	ss.Now = c.now
	s := ss.Start(Admin{ID: "a", Name: "ops"}, false)
	if !s.Fresh(c.t) {
		t.Fatal("fresh at sign-in")
	}
	c.t = c.t.Add(6 * time.Minute)
	if ss.Get(s.Token) == nil || s.Fresh(c.t) {
		t.Fatal("alive, not fresh")
	}
	ss.Reauth(s.Token)
	if !s.Fresh(c.t) {
		t.Fatal("re-authenticated")
	}
	c.t = c.t.Add(31 * time.Minute)
	if ss.Get(s.Token) != nil {
		t.Fatal("idle sessions end")
	}
	s2 := ss.Start(Admin{ID: "a"}, false)
	for i := 0; i < 30; i++ {
		c.t = c.t.Add(25 * time.Minute)
		ss.Get(s2.Token)
	}
	if ss.Get(s2.Token) != nil {
		t.Fatal("sessions end after 12 hours however active")
	}
	l := NewLimiter(5, 15*time.Minute)
	l.Now = c.now
	for i := 0; i < 5; i++ {
		l.Fail("1.2.3.4", "name:ops")
	}
	if ok, wait := l.Allowed("1.2.3.4"); ok || wait <= 0 {
		t.Fatal("limited")
	}
	if ok, _ := l.Allowed("5.6.7.8"); !ok {
		t.Fatal("per key")
	}
	c.t = c.t.Add(16 * time.Minute)
	if ok, _ := l.Allowed("1.2.3.4", "name:ops"); !ok {
		t.Fatal("the window passes")
	}
}
