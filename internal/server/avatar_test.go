package server_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"onegit/internal/store"
)

var onePixelPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

var avatarSrcRe = regexp.MustCompile(`src="(/avatars/\d+\?v=\d+)"`)

func (b *browser) upload(path, field string, data []byte) page {
	b.h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile(field, "avatar.bin")
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", b.h.url+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return b.do(req)
}

func TestAvatars(t *testing.T) {
	t.Parallel()
	h := start(t, nil)
	admin := h.admin()
	dev := h.createUser(admin, "dev", "write")
	u, err := h.srv.Store.UserByUsername(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`src="/avatars/%d?v=`, u.ID)

	// Without an avatar, initials are drawn.
	if body := dev.ok("/settings", "Upload"); strings.Contains(body, src) {
		t.Error("avatar image shown before upload")
	}
	// Only safe image formats are accepted.
	dev.upload("/settings/avatar", "avatar", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	if _, err := h.srv.Store.Avatar(ctx, u.ID); err == nil {
		t.Error("SVG accepted as avatar")
	}
	if p := dev.upload("/settings/avatar", "avatar", onePixelPNG); p.status != http.StatusSeeOther {
		t.Fatalf("upload: %d", p.status)
	}
	if ok, _ := h.srv.Blob.Exists(ctx, fmt.Sprintf("avatars/%d", u.ID)); !ok {
		t.Error("avatar image not in S3")
	}
	m := avatarSrcRe.FindStringSubmatch(dev.ok("/settings", src, "Remove avatar"))
	if m == nil {
		t.Fatal("no avatar image on the settings page")
	}
	p := dev.get(m[1])
	if p.status != http.StatusOK || p.header.Get("Content-Type") != "image/png" || p.body != string(onePixelPNG) {
		t.Errorf("avatar image: %d %q", p.status, p.header.Get("Content-Type"))
	}
	// Other users see it too, e.g. in the admin list.
	admin.ok("/admin/users", src)

	if p := dev.post("/settings/avatar/delete"); p.status != http.StatusSeeOther {
		t.Errorf("remove: %d", p.status)
	}
	if _, err := h.srv.Store.Avatar(ctx, u.ID); err == nil {
		t.Error("avatar not removed")
	}
	if ok, _ := h.srv.Blob.Exists(ctx, fmt.Sprintf("avatars/%d", u.ID)); ok {
		t.Error("avatar image left in S3")
	}

	// An avatar from the identity provider is read-only.
	if err := h.srv.Avatars.Set(ctx, u.ID, store.AvatarOIDC, onePixelPNG); err != nil {
		t.Fatal(err)
	}
	if body := dev.ok("/settings", "Change it there"); strings.Contains(body, `action="/settings/avatar"`) {
		t.Error("upload form shown for an IdP avatar")
	}
	dev.upload("/settings/avatar", "avatar", onePixelPNG)
	dev.post("/settings/avatar/delete")
	if a, err := h.srv.Store.Avatar(ctx, u.ID); err != nil || a.Source != store.AvatarOIDC {
		t.Errorf("IdP avatar changed by the user: %+v, %v", a, err)
	}

	// Deleting the user deletes the image in S3.
	admin.post(fmt.Sprintf("/admin/users/%d/delete", u.ID))
	if ok, _ := h.srv.Blob.Exists(ctx, fmt.Sprintf("avatars/%d", u.ID)); ok {
		t.Error("avatar image of a deleted user left in S3")
	}
}
