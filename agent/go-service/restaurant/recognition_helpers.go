package restaurant

import (
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/iconrecognition"
	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/resource"
	maa "github.com/MaaXYZ/maa-framework-go/v4"
)

const (
	restaurantDataPath       = "data/Restaurant/restaurant.json"
	restaurantPlanNode       = "RestaurantPlanMenu"
	restaurantRuntimeNode    = "RestaurantRuntimeNextKitchen"
	restaurantMenuSelectNode = "RestaurantMenuSelectDishNext"
	kitchensAttach           = "kitchens"
)

var restaurantScrollNodes = [...]string{
	"RestaurantMenuSelectDishIsAtTop",
	"RestaurantMenuSelectDishIsAtBottom",
	"RestaurantMenuSelectDrinkIsAtTop",
	"RestaurantMenuSelectDrinkIsAtBottom",
}

var (
	restaurantData     Data
	restaurantDataOnce sync.Once
	restaurantDataErr  error
)

func loadRestaurantData() (Data, error) {
	restaurantDataOnce.Do(func() {
		restaurantDataErr = resource.ReadJsonResource(restaurantDataPath, &restaurantData)
		if restaurantDataErr == nil && (len(restaurantData.Dishes) == 0 || len(restaurantData.Drinks) == 0) {
			restaurantDataErr = fmt.Errorf("restaurant data is missing dishes or drinks")
		}
	})
	if restaurantDataErr != nil {
		return Data{}, fmt.Errorf("read restaurant data: %w", restaurantDataErr)
	}
	return restaurantData, nil
}

type ocrText struct {
	Text string
	Box  maa.Rect
}

func recognizeOCR(ctx *maa.Context, img image.Image, roi maa.Rect) ([]ocrText, error) {
	param := &maa.OCRParam{
		ROI:      maa.NewTargetRect(roi),
		Expected: []string{".*"},
		OnlyRec:  false,
	}
	detail, err := ctx.RunRecognitionDirect(maa.RecognitionTypeOCR, param, img)
	if err != nil {
		return nil, err
	}
	if detail == nil || detail.Results == nil {
		return nil, nil
	}
	results := detail.Results.Filtered
	texts := make([]ocrText, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		if result == nil {
			continue
		}
		textResult, ok := result.AsOCR()
		if !ok || textResult == nil {
			continue
		}
		text := strings.TrimSpace(textResult.Text)
		if text == "" {
			continue
		}
		key := fmt.Sprintf("%s:%v", text, textResult.Box)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		texts = append(texts, ocrText{Text: text, Box: textResult.Box})
	}
	return texts, nil
}

func matchIcon(ctx *maa.Context, img image.Image, roi maa.Rect, itemIDs []string) (string, error) {
	if len(itemIDs) == 0 {
		return "", nil
	}
	region := logicalRect(img, roi)
	if region.Empty() {
		return "", fmt.Errorf("icon ROI %v is outside the image", roi)
	}
	// Keep the matcher's two-pixel search and subpixel sampling inside the crop.
	bounds := region.Inset(-4).Intersect(img.Bounds())
	cropped := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(cropped, cropped.Bounds(), img, bounds.Min, draw.Src)
	localROI := maa.Rect{region.Min.X - bounds.Min.X, region.Min.Y - bounds.Min.Y, region.Dx(), region.Dy()}
	detail, err := ctx.RunRecognitionDirect(
		maa.RecognitionTypeCustom,
		&maa.CustomRecognitionParam{
			ROI:               maa.NewTargetRect(localROI),
			CustomRecognition: iconrecognition.CustomRecognitionName,
			CustomRecognitionParam: iconrecognition.NewParams(
				iconrecognition.WithGridType(iconrecognition.GridTypeSingleROI),
				iconrecognition.WithItemFilters(iconrecognition.ItemFilter("Restaurant:*"), iconrecognition.StorageFilter().Normal.Usable),
				iconrecognition.WithItemIDs(itemIDs...),
			),
		},
		cropped,
	)
	if err != nil {
		return "", err
	}
	parsed, _, err := iconrecognition.ParseRecognitionDetail(detail)
	if err != nil {
		return "", err
	}
	if parsed.Error != nil && parsed.Error.Code != iconrecognition.ErrorCodeNoMatch {
		return "", fmt.Errorf("IconRecognition %s: %s", parsed.Error.Code, parsed.Error.Message)
	}
	if !parsed.Matched || len(parsed.Matches) == 0 {
		return "", nil
	}
	return parsed.Matches[0].ItemID, nil
}

func readAttach[T any](ctx *maa.Context, nodeName, key string) (T, bool, error) {
	var value T
	raw, err := ctx.GetNodeJSON(nodeName)
	if err != nil {
		return value, false, err
	}
	var node struct {
		Attach map[string]json.RawMessage `json:"attach"`
	}
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		return value, false, err
	}
	rawValue, exists := node.Attach[key]
	if !exists || len(rawValue) == 0 || string(rawValue) == "null" {
		return value, false, nil
	}
	if err := json.Unmarshal(rawValue, &value); err != nil {
		return value, false, fmt.Errorf("decode %s.attach.%s: %w", nodeName, key, err)
	}
	return value, true, nil
}

func writeAttach(ctx *maa.Context, nodeName, key string, value any) error {
	raw, err := ctx.GetNodeJSON(nodeName)
	if err != nil {
		return err
	}
	var node struct {
		Attach map[string]any `json:"attach"`
	}
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		return err
	}
	if node.Attach == nil {
		node.Attach = make(map[string]any)
	}
	node.Attach[key] = value
	return ctx.OverridePipeline(map[string]any{
		nodeName: map[string]any{
			"attach": node.Attach,
		},
	})
}

func logicalRect(img image.Image, rect maa.Rect) image.Rectangle {
	bounds := img.Bounds()
	if bounds.Empty() || bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return image.Rectangle{}
	}
	scaleX := float64(bounds.Dx()) / 1280
	scaleY := float64(bounds.Dy()) / 720
	left := bounds.Min.X + int(float64(rect[0])*scaleX)
	top := bounds.Min.Y + int(float64(rect[1])*scaleY)
	right := bounds.Min.X + int(float64(rect[0]+rect[2])*scaleX+0.999)
	bottom := bounds.Min.Y + int(float64(rect[1]+rect[3])*scaleY+0.999)
	return image.Rect(left, top, right, bottom).Intersect(bounds)
}

func logicalPixel(img image.Image, x, y int) (uint8, uint8, uint8, bool) {
	rect := logicalRect(img, maa.Rect{x, y, 1, 1})
	if rect.Empty() {
		return 0, 0, 0, false
	}
	r, g, b, _ := img.At(rect.Min.X, rect.Min.Y).RGBA()
	return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), true
}

func yellowPixelCount(img image.Image, rect maa.Rect) int {
	count := 0
	for y := rect[1]; y < rect[1]+rect[3]; y++ {
		for x := rect[0]; x < rect[0]+rect[2]; x++ {
			r, g, b, ok := logicalPixel(img, x, y)
			if ok && int(r) > 165 && int(g) > 105 && int(r)-int(b) > 35 && int(g)-int(b) > 20 {
				count++
			}
		}
	}
	return count
}

func isMenuCardSelected(img image.Image, card maa.Rect) bool {
	badge := menuSelectionBadge(card)
	for dy := -5; dy <= 5; dy++ {
		for dx := -5; dx <= 5; dx++ {
			candidate := badge
			candidate[0] += dx
			candidate[1] += dy
			if isMenuMinusBadge(img, candidate) {
				return true
			}
		}
	}
	return false
}

func isMenuMinusBadge(img image.Image, badge maa.Rect) bool {
	dark := 0
	for y := badge[1]; y < badge[1]+badge[3]; y++ {
		for x := badge[0]; x < badge[0]+badge[2]; x++ {
			r, g, b, ok := logicalPixel(img, x, y)
			if ok && max(r, g, b) < 120 {
				dark++
			}
		}
	}
	line := 0
	for x := badge[0] + 5; x < badge[0]+badge[2]-5; x++ {
		r, g, b, ok := logicalPixel(img, x, badge[1]+badge[3]/2)
		if ok && min(r, g, b) > 180 {
			line++
		}
	}
	if dark < 80 || line < 4 {
		return false
	}
	cx, cy := badge[0]+badge[2]/2, badge[1]+badge[3]/2
	for _, point := range [][2]int{{cx, cy - 5}, {cx, cy + 5}, {cx - 7, cy}, {cx + 7, cy}} {
		r, g, b, ok := logicalPixel(img, point[0], point[1])
		if !ok || max(r, g, b) >= 120 {
			return false
		}
	}
	return true
}

func menuSelectionBadge(card maa.Rect) maa.Rect {
	return maa.Rect{card[0] + card[2] - 15, card[1] + 2, 19, 19}
}

type visibleMenuCard struct {
	Box       maa.Rect
	Icon      maa.Rect
	Available bool
}

var menuQuantityPattern = regexp.MustCompile(`(?:可烹饪|库存)\s*(∞|\d+)`)

// Visible availability labels locate rows after arbitrary scrolling; partial
// cards are revisited on an overlapping page instead of using fixed row slots.
func visibleMenuCards(ctx *maa.Context, img image.Image, kind string) ([]visibleMenuCard, error) {
	texts, err := recognizeOCR(ctx, img, maa.Rect{552, 133, 640, 404})
	if err != nil {
		return nil, err
	}
	prefix := "可烹饪"
	if kind == "drink" {
		prefix = "库存"
	}
	cards := make([]visibleMenuCard, 0, 8)
	for _, text := range texts {
		if !strings.Contains(text.Text, prefix) {
			continue
		}
		x := menuCardsX[0]
		if text.Box[0] >= 860 {
			x = menuCardsX[1]
		}
		// OCR boxes can grow upwards; anchor the card to the text's center.
		y := text.Box[1] + text.Box[3]/2 - 78
		icon := maa.Rect{x + 34, y + 14, 64, 64}
		if icon[1] < 134 || icon[1]+icon[3] > 536 {
			continue
		}
		match := menuQuantityPattern.FindStringSubmatch(strings.ReplaceAll(text.Text, " ", ""))
		if len(match) != 2 {
			return nil, fmt.Errorf("menu availability quantity was not recognized: %q", text.Text)
		}
		available := match[1] == "∞"
		if !available {
			quantity, _ := strconv.Atoi(match[1])
			available = quantity > 0
		}
		cards = append(cards, visibleMenuCard{Box: maa.Rect{x, y, menuCardWidth, menuCardHeight}, Icon: icon, Available: available})
	}
	if len(cards) == 0 {
		return nil, fmt.Errorf("no complete %s menu cards were recognized", kind)
	}
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].Box[1] != cards[j].Box[1] {
			return cards[i].Box[1] < cards[j].Box[1]
		}
		return cards[i].Box[0] < cards[j].Box[0]
	})
	// Group small OCR height differences into rows before ordering each row left to right.
	for first := 0; first < len(cards); {
		last := first + 1
		for last < len(cards) && cards[last].Box[1]-cards[first].Box[1] < menuCardHeight/2 {
			last++
		}
		row := cards[first:last]
		sort.SliceStable(row, func(i, j int) bool { return row[i].Box[0] < row[j].Box[0] })
		first = last
	}
	return cards, nil
}
