package external

import "testing"

func TestWikimediaFileTitle(t *testing.T) {
	cases := []struct {
		name, url, want string
	}{
		{"commons thumb", "https://upload.wikimedia.org/wikipedia/commons/thumb/3/39/Phad_thai_edit.jpg/800px-Phad_thai_edit.jpg", "Phad_thai_edit.jpg"},
		{"rest summary thumb", "https://thumb.wikimedia.org/wikipedia/commons/thumb/a/bc/Sule_Pagoda.jpg/330px-Sule_Pagoda.jpg", "Sule_Pagoda.jpg"},
		{"direct original", "https://upload.wikimedia.org/wikipedia/en/9/97/Tom_yum_goong.jpg", "Tom_yum_goong.jpg"},
		{"non wikimedia", "https://img.example.com/place.jpg", ""},
		{"map path", "/api/v1/media/map?lon=96.1&lat=16.8", ""},
	}
	for _, c := range cases {
		if got := WikimediaFileTitle(c.url); got != c.want {
			t.Errorf("%s: WikimediaFileTitle(%q) = %q, want %q", c.name, c.url, got, c.want)
		}
	}
}

func TestPhotoSubjectJunk(t *testing.T) {
	junk := []string{
		"Yangon_locator_map.jpg", "Paris_map.svg", "Seal_of_Bangkok.png",
		"Coat_of_arms_of_Rome.svg", "Tokyo_plan_1889.jpg", "Street_plan_of_London.png",
		"Shwedagon_1895.png", "Old_Pagoda_1903_postcard.jpg", "logo.png",
	}
	for _, title := range junk {
		if !PhotoSubjectJunk(title) {
			t.Errorf("PhotoSubjectJunk(%q) = false, want true", title)
		}
	}
	keep := []string{
		"Phad_thai.jpg", "Sule_Pagoda.jpg", "U_Bein_Bridge_sunset.jpg",
		"Hotel_de_Matignon_2019.jpg", "Bagan_Temples_panorama.jpg",
	}
	for _, title := range keep {
		if PhotoSubjectJunk(title) {
			t.Errorf("PhotoSubjectJunk(%q) = true, want false", title)
		}
	}
	if PhotoSubjectJunk("") {
		t.Error("empty title must not be junk (no signal)")
	}
}
