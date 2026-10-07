package dijiangrewards

import (
	"image"
	"image/color"
	"testing"

	maa "github.com/MaaXYZ/maa-framework-go/v4"
)

func TestClassifyRecoveryEmotionBandColor(t *testing.T) {
	tests := []struct {
		name string
		rgb  [3]int
		want recoveryEmotionBandColor
	}{
		{name: "control nexus", rgb: [3]int{52, 93, 121}, want: recoveryEmotionBandControlNexus},
		{name: "reception room", rgb: [3]int{93, 81, 148}, want: recoveryEmotionBandReception},
		{name: "manufacturing", rgb: [3]int{144, 137, 24}, want: recoveryEmotionBandManufacture},
		{name: "growth chamber", rgb: [3]int{109, 148, 28}, want: recoveryEmotionBandGrowth},
		{name: "unassigned", rgb: [3]int{45, 46, 44}, want: recoveryEmotionBandNeutral},
		{name: "warm tinted unassigned", rgb: [3]int{50, 42, 37}, want: recoveryEmotionBandNeutral},
		{name: "unknown red", rgb: [3]int{190, 45, 45}, want: recoveryEmotionBandUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyRecoveryEmotionBandColor(test.rgb[0], test.rgb[1], test.rgb[2]); got != test.want {
				t.Fatalf("classifyRecoveryEmotionBandColor(%v) = %q, want %q", test.rgb, got, test.want)
			}
		})
	}
}

func TestRecoveryEmotionBandColorIgnoresUniformBrightness(t *testing.T) {
	colors := [][3]int{
		{52, 93, 121},
		{93, 81, 148},
		{144, 137, 24},
		{109, 148, 28},
	}
	for _, base := range colors {
		want := classifyRecoveryEmotionBandColor(base[0], base[1], base[2])
		for _, scale := range []float64{0.45, 0.7, 1.0} {
			rgb := [3]int{
				int(float64(base[0]) * scale),
				int(float64(base[1]) * scale),
				int(float64(base[2]) * scale),
			}
			if got := classifyRecoveryEmotionBandColor(rgb[0], rgb[1], rgb[2]); got != want {
				t.Fatalf("classification changed for %v at brightness %.2f: got %q, want %q", base, scale, got, want)
			}
		}
	}
}

func TestRecoveryEmotionBandColorToleratesWhiteBalanceShift(t *testing.T) {
	colors := [][3]int{
		{52, 93, 121},
		{93, 81, 148},
		{144, 137, 24},
		{109, 148, 28},
	}
	channelGains := [][3]float64{
		{1.05, 0.98, 0.95},
		{0.95, 1.05, 1.05},
	}
	for _, base := range colors {
		want := classifyRecoveryEmotionBandColor(base[0], base[1], base[2])
		for _, gains := range channelGains {
			rgb := [3]int{
				int(float64(base[0]) * gains[0]),
				int(float64(base[1]) * gains[1]),
				int(float64(base[2]) * gains[2]),
			}
			if got := classifyRecoveryEmotionBandColor(rgb[0], rgb[1], rgb[2]); got != want {
				t.Fatalf("classification changed for %v after channel gains %v: got %q, want %q", base, gains, got, want)
			}
		}
	}
}

func TestRecoveryEmotionOperatorBandColorSamplesBandEdges(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 40))
	band := image.Rect(10, 10, 91, 24)
	for y := band.Min.Y; y < band.Max.Y; y++ {
		for x := band.Min.X; x < band.Max.X; x++ {
			img.Set(x, y, color.RGBA{R: 80, G: 137, B: 27, A: 255})
		}
	}
	for y := band.Min.Y; y < band.Max.Y; y++ {
		for x := 28; x < 72; x++ {
			img.Set(x, y, color.RGBA{A: 255})
		}
	}

	if got := recoveryEmotionOperatorBandColor(img, maa.Rect{10, 10, 81, 14}); got != recoveryEmotionBandGrowth {
		t.Fatalf("recoveryEmotionOperatorBandColor() = %q, want %q", got, recoveryEmotionBandGrowth)
	}
}

func TestRecoveryEmotionOperatorCardsFollowShiftedBandRows(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	background := color.RGBA{R: 20, G: 20, B: 20, A: 255}
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			img.Set(x, y, background)
		}
	}
	bandColors := []color.RGBA{
		{R: 52, G: 93, B: 121, A: 255},
		{R: 93, G: 81, B: 148, A: 255},
		{R: 144, G: 137, B: 24, A: 255},
		{R: 109, G: 148, B: 28, A: 255},
	}
	const shift = 12
	for row, baseY := range []int{317, 457} {
		for column := 0; column < 6; column++ {
			band := image.Rect(540+column*90, baseY+shift, 540+column*90+81, baseY+shift+18)
			fillRecoveryEmotionTestRect(img, band, bandColors[(row+column)%len(bandColors)])
		}
	}

	cards := recoveryEmotionOperatorCards(img, maa.Rect{540, 240, 540, 280})
	if len(cards) != 12 {
		t.Fatalf("got %d cards, want 12", len(cards))
	}
	if cards[0].band != (maa.Rect{540, 329, 81, 18}) || cards[6].band != (maa.Rect{540, 469, 81, 18}) {
		t.Fatalf("dynamic band positions = %v and %v, want y=329 and y=469", cards[0].band, cards[6].band)
	}
	if cards[0].card[1] != 258 || cards[6].card[1] != 398 {
		t.Fatalf("dynamic card positions = y=%d and y=%d, want y=258 and y=398", cards[0].card[1], cards[6].card[1])
	}
	if cards[0].click != (maa.Rect{580, 315, 1, 1}) || cards[6].click != (maa.Rect{580, 455, 1, 1}) {
		t.Fatalf("unexpected shifted fixed click points: first=%v second-row=%v", cards[0].click, cards[6].click)
	}
}

func TestRecoveryEmotionOperatorCardSelectedFromCheckMarker(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 140))
	card := maa.Rect{10, 10, 81, 115}
	fillRecoveryEmotionTestRect(img, image.Rect(0, 0, 100, 140), color.RGBA{R: 35, G: 35, B: 35, A: 255})
	fillRecoveryEmotionTestRect(img, image.Rect(card[0]+card[2]-17, card[1]+2, card[0]+card[2]-2, card[1]+17), color.RGBA{R: 246, G: 236, B: 10, A: 255})
	if !recoveryEmotionOperatorCardSelected(img, card) {
		t.Fatal("yellow selection check marker was not detected")
	}
	fillRecoveryEmotionTestRect(img, image.Rect(card[0]+card[2]-17, card[1]+2, card[0]+card[2]-2, card[1]+17), color.RGBA{R: 75, G: 75, B: 75, A: 255})
	if recoveryEmotionOperatorCardSelected(img, card) {
		t.Fatal("neutral unselected corner was detected as selected")
	}
}

func TestMatchesRecoveryEmotionCabinRules(t *testing.T) {
	tests := []struct {
		name      string
		texts     []string
		bandColor recoveryEmotionBandColor
		cabins    map[string]recoveryEmotionCabinTarget
		want      bool
	}{
		{
			name:      "ordinary cabin can match by color",
			bandColor: recoveryEmotionBandReception,
			cabins: map[string]recoveryEmotionCabinTarget{
				"reception_room": {Labels: []string{"会客室"}, BandColor: recoveryEmotionBandReception},
			},
			want: true,
		},
		{
			name:      "ordinary cabin can match by exact name",
			texts:     []string{"会客室"},
			bandColor: recoveryEmotionBandUnknown,
			cabins: map[string]recoveryEmotionCabinTarget{
				"reception_room": {Labels: []string{"会客室"}, BandColor: recoveryEmotionBandReception},
			},
			want: true,
		},
		{
			name:      "manufacturing requires yellow and exact numeral",
			texts:     []string{"制造舱 II"},
			bandColor: recoveryEmotionBandManufacture,
			cabins: map[string]recoveryEmotionCabinTarget{
				"manufacturing_ii": {Labels: []string{"制造舱 II"}, BandColor: recoveryEmotionBandManufacture, Manufacturing: true},
			},
			want: true,
		},
		{
			name:      "manufacturing I matches the observed OCR digit one alias",
			texts:     []string{"制造舱1"},
			bandColor: recoveryEmotionBandManufacture,
			cabins: map[string]recoveryEmotionCabinTarget{
				"manufacturing_i": {Labels: []string{"制造舱 I", "制造舱1"}, BandColor: recoveryEmotionBandManufacture, Manufacturing: true},
			},
			want: true,
		},
		{
			name:      "manufacturing I remains selectable with manufacturing II enabled",
			texts:     []string{"制造舱1"},
			bandColor: recoveryEmotionBandManufacture,
			cabins: map[string]recoveryEmotionCabinTarget{
				"manufacturing_i":  {Labels: []string{"制造舱 I", "制造舱1"}, BandColor: recoveryEmotionBandManufacture, Manufacturing: true},
				"manufacturing_ii": {Labels: []string{"制造舱 II", "制造舱Ⅱ"}, BandColor: recoveryEmotionBandManufacture, Manufacturing: true},
			},
			want: true,
		},
		{
			name:      "manufacturing I does not match manufacturing II",
			texts:     []string{"制造舱Ⅱ"},
			bandColor: recoveryEmotionBandManufacture,
			cabins: map[string]recoveryEmotionCabinTarget{
				"manufacturing_i": {Labels: []string{"制造舱 I", "制造舱1"}, BandColor: recoveryEmotionBandManufacture, Manufacturing: true},
			},
			want: false,
		},
		{
			name:      "manufacturing without numeral is rejected",
			texts:     []string{"制造舱"},
			bandColor: recoveryEmotionBandManufacture,
			cabins: map[string]recoveryEmotionCabinTarget{
				"manufacturing_i": {Labels: []string{"制造舱 I"}, BandColor: recoveryEmotionBandManufacture, Manufacturing: true},
			},
			want: false,
		},
		{
			name:      "manufacturing numeral without yellow is rejected",
			texts:     []string{"制造舱 I"},
			bandColor: recoveryEmotionBandUnknown,
			cabins: map[string]recoveryEmotionCabinTarget{
				"manufacturing_i": {Labels: []string{"制造舱 I"}, BandColor: recoveryEmotionBandManufacture, Manufacturing: true},
			},
			want: false,
		},
		{
			name:      "unselected cabin does not match by color",
			bandColor: recoveryEmotionBandControlNexus,
			cabins: map[string]recoveryEmotionCabinTarget{
				"reception_room": {Labels: []string{"会客室"}, BandColor: recoveryEmotionBandReception},
			},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesRecoveryEmotionCabin(test.texts, test.bandColor, test.cabins); got != test.want {
				t.Fatalf("matchesRecoveryEmotionCabin() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestIsRecoveryEmotionUnassigned(t *testing.T) {
	if !isRecoveryEmotionUnassigned(nil, recoveryEmotionBandNeutral) {
		t.Fatal("neutral band should identify an unassigned card")
	}
	if !isRecoveryEmotionUnassigned([]string{"无安排"}, recoveryEmotionBandUnknown) {
		t.Fatal("exact unassigned text should identify an unassigned card")
	}
	if isRecoveryEmotionUnassigned([]string{"200%"}, recoveryEmotionBandUnknown) {
		t.Fatal("trust text alone must not identify an unassigned card")
	}
}

func TestRecoveryEmotionOperatorCardsUseCabinNameStrip(t *testing.T) {
	cards := recoveryEmotionOperatorCards(nil, maa.Rect{540, 240, 540, 280})
	if len(cards) != 12 {
		t.Fatalf("got %d cards, want 12", len(cards))
	}
	if cards[0].band != (maa.Rect{540, 317, 81, 18}) || cards[6].band != (maa.Rect{540, 457, 81, 18}) {
		t.Fatalf("unexpected cabin band regions: first=%v second-row=%v", cards[0].band, cards[6].band)
	}
	if cards[0].label != (maa.Rect{540, 317, 81, 16}) || cards[6].label != (maa.Rect{540, 457, 81, 16}) {
		t.Fatalf("unexpected cabin label OCR regions: first=%v second-row=%v", cards[0].label, cards[6].label)
	}
	if cards[0].card[1] != 246 || cards[6].card[1] != 386 {
		t.Fatalf("unexpected card positions: first=%v second-row=%v", cards[0].card, cards[6].card)
	}
	if cards[0].click != (maa.Rect{580, 303, 1, 1}) || cards[6].click != (maa.Rect{580, 443, 1, 1}) {
		t.Fatalf("unexpected fixed click points: first=%v second-row=%v", cards[0].click, cards[6].click)
	}
}

func fillRecoveryEmotionTestRect(img *image.RGBA, rect image.Rectangle, fill color.RGBA) {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			img.Set(x, y, fill)
		}
	}
}
