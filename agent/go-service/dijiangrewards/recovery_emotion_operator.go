package dijiangrewards

import (
	"encoding/json"
	"fmt"
	"image"
	"sort"
	"strings"

	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

const (
	recoveryEmotionOperatorRecognition = "RecoveryEmotionCabinOperatorRecognition"
	recoveryEmotionCabinAttachNode     = "RecoveryEmotionSelectLowEmotion"
	recoveryEmotionOCRThreshold        = 0.15
	recoveryEmotionFirstBandOffset     = 77
	recoveryEmotionSecondBandOffset    = 217
	recoveryEmotionBandSearchMargin    = 60
	recoveryEmotionBandMinimumRun      = 14
	recoveryEmotionBandHeight          = 18
	recoveryEmotionLabelTextHeight     = 16
	recoveryEmotionNeutralChromaRatio  = 0.35
	recoveryEmotionYellowChannelRatio  = 0.2
)

type recoveryEmotionBandColor string

const (
	recoveryEmotionBandUnknown      recoveryEmotionBandColor = "unknown"
	recoveryEmotionBandNeutral      recoveryEmotionBandColor = "neutral"
	recoveryEmotionBandControlNexus recoveryEmotionBandColor = "control_nexus"
	recoveryEmotionBandReception    recoveryEmotionBandColor = "reception_room"
	recoveryEmotionBandManufacture  recoveryEmotionBandColor = "manufacturing"
	recoveryEmotionBandGrowth       recoveryEmotionBandColor = "growth_chamber"
)

var recoveryEmotionUnassignedLabels = []string{
	"无安排",
	"無安排",
	"Unassigned",
	"未配置",
	"미배치",
}

type recoveryEmotionOperatorRecognitionParam struct {
	Mode        string `json:"mode"`
	TargetIndex int    `json:"target_index,omitempty"`
}

type recoveryEmotionCabinAttach struct {
	Attach map[string][]string `json:"attach"`
}

type recoveryEmotionCabinTarget struct {
	Labels        []string
	BandColor     recoveryEmotionBandColor
	Manufacturing bool
}

type recoveryEmotionOperatorCard struct {
	row    int
	column int
	label  maa.Rect
	band   maa.Rect
	card   maa.Rect
	click  maa.Rect
}

// RecoveryEmotionOperatorRecognition scans the visible cards from left to right and top to bottom.
type RecoveryEmotionOperatorRecognition struct{}

var _ maa.CustomRecognitionRunner = &RecoveryEmotionOperatorRecognition{}

func (r *RecoveryEmotionOperatorRecognition) Run(
	ctx *maa.Context,
	arg *maa.CustomRecognitionArg,
) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil || arg.Img == nil {
		return nil, false
	}
	if arg.Roi[2] <= 0 || arg.Roi[3] <= 0 {
		log.Error().Str("component", recoveryEmotionOperatorRecognition).Msg("recognition roi is empty")
		return nil, false
	}

	var params recoveryEmotionOperatorRecognitionParam
	if err := json.Unmarshal([]byte(arg.CustomRecognitionParam), &params); err != nil {
		log.Error().Err(err).Str("component", recoveryEmotionOperatorRecognition).Msg("failed to parse params")
		return nil, false
	}
	if params.Mode != "target" && params.Mode != "unassigned" {
		log.Error().Str("component", recoveryEmotionOperatorRecognition).Str("mode", params.Mode).Msg("unsupported scan mode")
		return nil, false
	}
	if params.Mode == "target" && (params.TargetIndex < 0 || params.TargetIndex > 1) {
		log.Error().Str("component", recoveryEmotionOperatorRecognition).Int("target_index", params.TargetIndex).Msg("target index must be zero or one")
		return nil, false
	}

	var cabins map[string]recoveryEmotionCabinTarget
	if params.Mode == "target" {
		var err error
		cabins, err = loadRecoveryEmotionCabinTargets(ctx)
		if err != nil {
			log.Error().Err(err).Str("component", recoveryEmotionOperatorRecognition).Msg("failed to load selected cabins")
			return nil, false
		}
	}

	targetCount := 0
	for _, card := range recoveryEmotionOperatorCards(arg.Img, arg.Roi) {
		bandColor := recoveryEmotionOperatorBandColor(arg.Img, card.band)
		texts, err := recognizeRecoveryEmotionCard(ctx, arg.Img, card.label)
		if err != nil {
			log.Debug().Err(err).Str("component", recoveryEmotionOperatorRecognition).Int("row", card.row+1).Int("column", card.column+1).Msg("card label OCR failed")
			texts = nil
		}
		selected := recoveryEmotionOperatorCardSelected(arg.Img, card.card)
		log.Debug().
			Str("component", recoveryEmotionOperatorRecognition).
			Str("mode", params.Mode).
			Strs("ocr_texts", texts).
			Str("band_color", string(bandColor)).
			Bool("selected", selected).
			Int("band_y", card.band[1]).
			Int("row", card.row+1).
			Int("column", card.column+1).
			Msg("scanned recovery emotion operator card")

		if isRecoveryEmotionUnassigned(texts, bandColor) {
			log.Debug().Str("component", recoveryEmotionOperatorRecognition).Str("mode", params.Mode).Str("band_color", string(bandColor)).Int("row", card.row+1).Int("column", card.column+1).Msg("found first unassigned card")
			if params.Mode == "unassigned" {
				return recoveryEmotionOperatorResult(card, params), true
			}
			return nil, false
		}

		if params.Mode != "target" {
			continue
		}
		if matchesRecoveryEmotionCabin(texts, bandColor, cabins) {
			if selected {
				targetCount++
				continue
			}
			if targetCount == params.TargetIndex {
				log.Debug().Str("component", recoveryEmotionOperatorRecognition).Str("band_color", string(bandColor)).Int("target_index", params.TargetIndex).Int("row", card.row+1).Int("column", card.column+1).Msg("found matching cabin operator")
				return recoveryEmotionOperatorResult(card, params), true
			}
			targetCount++
		}
	}

	log.Debug().Str("component", recoveryEmotionOperatorRecognition).Str("mode", params.Mode).Int("target_index", params.TargetIndex).Int("target_count", targetCount).Msg("no matching card found on current page")
	return nil, false
}

func recoveryEmotionOperatorCards(img image.Image, roi maa.Rect) []recoveryEmotionOperatorCard {
	const (
		columns    = 6
		columnStep = 90
		labelWidth = 81
		cardHeight = 115
	)
	bandRows := [...]int{
		roi[1] + recoveryEmotionFirstBandOffset,
		roi[1] + recoveryEmotionSecondBandOffset,
	}
	shifts := [2]int{}
	found := [2]bool{}
	for row, bandY := range bandRows {
		shifts[row], found[row] = recoveryEmotionOperatorBandShift(img, roi, bandY)
	}
	if !found[0] && found[1] {
		shifts[0] = shifts[1]
	} else if found[0] && !found[1] {
		shifts[1] = shifts[0]
	}

	cards := make([]recoveryEmotionOperatorCard, 0, len(bandRows)*columns)
	for row, bandY := range bandRows {
		bandY += shifts[row]
		for column := 0; column < columns; column++ {
			x := roi[0] + column*columnStep
			band := maa.Rect{x, bandY, labelWidth, recoveryEmotionBandHeight}
			label := maa.Rect{x, bandY, labelWidth, recoveryEmotionLabelTextHeight}
			card := maa.Rect{x, bandY - 71, labelWidth, cardHeight}
			visible := image.Rect(card[0], card[1], card[0]+card[2], card[1]+card[3]).Intersect(image.Rect(roi[0], roi[1], roi[0]+roi[2], roi[1]+roi[3]))
			if img != nil {
				visible = visible.Intersect(img.Bounds())
			}
			if visible.Empty() {
				continue
			}
			clickX := x + labelWidth/2
			clickY := card[1] + cardHeight/2
			if !image.Pt(clickX, clickY).In(visible) {
				continue
			}
			// Keep Click on one fixed center pixel, away from the card's top-left info icon.
			cards = append(cards, recoveryEmotionOperatorCard{
				row:    row,
				column: column,
				label:  label,
				band:   band,
				card:   card,
				click:  maa.Rect{clickX, clickY, 1, 1},
			})
		}
	}
	return cards
}

func recoveryEmotionOperatorBandShift(img image.Image, roi maa.Rect, expectedY int) (int, bool) {
	if img == nil || roi[2] <= 0 || roi[3] < recoveryEmotionBandHeight {
		return 0, false
	}
	const (
		columns    = 6
		columnStep = 90
		labelWidth = 81
	)
	startY := max(roi[1], expectedY-recoveryEmotionBandSearchMargin)
	endY := min(roi[1]+roi[3]-1, expectedY+recoveryEmotionBandSearchMargin)
	if startY > endY {
		return 0, false
	}

	var shifts []int
	for column := 0; column < columns; column++ {
		x := roi[0] + column*columnStep
		lastColor := recoveryEmotionBandUnknown
		runStart := startY
		runLength := 0
		flushRun := func() {
			if lastColor != recoveryEmotionBandUnknown && lastColor != recoveryEmotionBandNeutral && runLength >= recoveryEmotionBandMinimumRun {
				bandY := runStart + (runLength-recoveryEmotionBandHeight)/2
				shifts = append(shifts, bandY-expectedY)
			}
			runLength = 0
		}
		for y := startY; y <= endY; y++ {
			color := recoveryEmotionOperatorBandColorAtY(img, x, y, labelWidth)
			if color == lastColor && color != recoveryEmotionBandUnknown && color != recoveryEmotionBandNeutral {
				runLength++
				continue
			}
			flushRun()
			lastColor = color
			runStart = y
			if color != recoveryEmotionBandUnknown && color != recoveryEmotionBandNeutral {
				runLength = 1
			}
		}
		flushRun()
	}
	if len(shifts) == 0 {
		return 0, false
	}

	// Group per-column detections around the most common offset; isolated image colors do not move the grid.
	best := make([]int, 0, len(shifts))
	for _, candidate := range shifts {
		cluster := make([]int, 0, len(shifts))
		for _, shift := range shifts {
			if recoveryEmotionAbs(shift-candidate) <= 2 {
				cluster = append(cluster, shift)
			}
		}
		sort.Ints(cluster)
		if len(cluster) > len(best) || (len(cluster) == len(best) && len(cluster) > 0 && recoveryEmotionAbs(cluster[len(cluster)/2]) < recoveryEmotionAbs(best[len(best)/2])) {
			best = cluster
		}
	}
	sort.Ints(best)
	return best[len(best)/2], true
}

func recoveryEmotionOperatorBandColorAtY(img image.Image, x, y, width int) recoveryEmotionBandColor {
	if img == nil || width < 20 || y < img.Bounds().Min.Y || y >= img.Bounds().Max.Y {
		return recoveryEmotionBandUnknown
	}
	band := image.Rect(x, y, x+width, y+1).Intersect(img.Bounds())
	if band.Dx() < 20 {
		return recoveryEmotionBandUnknown
	}
	sideWidth := min(9, band.Dx()/4)
	channels := [3][]int{}
	for _, xRange := range [][2]int{
		{band.Min.X + 2, band.Min.X + sideWidth},
		{band.Max.X - sideWidth, band.Max.X - 2},
	} {
		for sampleX := xRange[0]; sampleX < xRange[1]; sampleX++ {
			r, g, b, a := img.At(sampleX, y).RGBA()
			if a == 0 {
				continue
			}
			channels[0] = append(channels[0], int(r>>8))
			channels[1] = append(channels[1], int(g>>8))
			channels[2] = append(channels[2], int(b>>8))
		}
	}
	if len(channels[0]) == 0 {
		return recoveryEmotionBandUnknown
	}
	medians := [3]int{}
	for channel := range channels {
		sort.Ints(channels[channel])
		medians[channel] = channels[channel][len(channels[channel])/2]
	}
	return classifyRecoveryEmotionBandColor(medians[0], medians[1], medians[2])
}

func loadRecoveryEmotionCabinTargets(ctx *maa.Context) (map[string]recoveryEmotionCabinTarget, error) {
	raw, err := ctx.GetNodeJSON(recoveryEmotionCabinAttachNode)
	if err != nil {
		return nil, fmt.Errorf("get cabin attach from %s: %w", recoveryEmotionCabinAttachNode, err)
	}
	var node recoveryEmotionCabinAttach
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		return nil, fmt.Errorf("parse cabin attach from %s: %w", recoveryEmotionCabinAttachNode, err)
	}

	cabins := make(map[string]recoveryEmotionCabinTarget, len(node.Attach))
	for key, values := range node.Attach {
		labels := make([]string, 0, len(values))
		seen := make(map[string]struct{}, len(values))
		for _, value := range values {
			label := strings.TrimSpace(value)
			if label == "" {
				continue
			}
			if _, ok := seen[label]; ok {
				continue
			}
			seen[label] = struct{}{}
			labels = append(labels, label)
		}
		if len(labels) == 0 {
			continue
		}
		cabins[key] = recoveryEmotionCabinTarget{
			Labels:        labels,
			BandColor:     recoveryEmotionCabinBandColorForKey(key),
			Manufacturing: strings.HasPrefix(key, "manufacturing_"),
		}
	}
	if len(cabins) == 0 {
		return nil, fmt.Errorf("no cabin labels configured on %s", recoveryEmotionCabinAttachNode)
	}
	return cabins, nil
}

func recoveryEmotionCabinBandColorForKey(key string) recoveryEmotionBandColor {
	switch key {
	case "control_nexus":
		return recoveryEmotionBandControlNexus
	case "reception_room":
		return recoveryEmotionBandReception
	case "manufacturing_i", "manufacturing_ii":
		return recoveryEmotionBandManufacture
	case "growth_chamber_i":
		return recoveryEmotionBandGrowth
	default:
		return recoveryEmotionBandUnknown
	}
}

func matchesRecoveryEmotionCabin(
	texts []string,
	bandColor recoveryEmotionBandColor,
	cabins map[string]recoveryEmotionCabinTarget,
) bool {
	for _, cabin := range cabins {
		nameMatched := matchesRecoveryEmotionLabel(texts, cabin.Labels)
		if cabin.Manufacturing {
			if bandColor == recoveryEmotionBandManufacture && nameMatched {
				return true
			}
			continue
		}
		if nameMatched || (cabin.BandColor != recoveryEmotionBandUnknown && bandColor == cabin.BandColor) {
			return true
		}
	}
	return false
}

func isRecoveryEmotionUnassigned(texts []string, bandColor recoveryEmotionBandColor) bool {
	return bandColor == recoveryEmotionBandNeutral || matchesRecoveryEmotionLabel(texts, recoveryEmotionUnassignedLabels)
}

func recognizeRecoveryEmotionCard(ctx *maa.Context, img image.Image, roi maa.Rect) ([]string, error) {
	detail, err := ctx.RunRecognitionDirect(maa.RecognitionTypeOCR, &maa.OCRParam{
		ROI:       maa.NewTargetRect(roi),
		Threshold: recoveryEmotionOCRThreshold,
	}, img)
	if err != nil {
		return nil, err
	}
	if detail == nil || detail.Results == nil {
		return nil, nil
	}

	results := detail.Results.All
	if len(results) == 0 {
		results = detail.Results.Filtered
	}
	if len(results) == 0 && detail.Results.Best != nil {
		results = []*maa.RecognitionResult{detail.Results.Best}
	}

	texts := make([]string, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		if result == nil {
			continue
		}
		ocr, ok := result.AsOCR()
		if !ok || ocr == nil {
			continue
		}
		text := strings.TrimSpace(ocr.Text)
		if text == "" {
			continue
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		texts = append(texts, text)
	}
	return texts, nil
}

func matchesRecoveryEmotionLabel(texts, labels []string) bool {
	for _, text := range texts {
		for _, label := range labels {
			if strings.EqualFold(text, label) {
				return true
			}
		}
	}
	return false
}

func recoveryEmotionOperatorBandColor(img image.Image, roi maa.Rect) recoveryEmotionBandColor {
	if img == nil {
		return recoveryEmotionBandUnknown
	}
	band := image.Rect(roi[0], roi[1], roi[0]+roi[2], roi[1]+roi[3]).Intersect(img.Bounds())
	if band.Dx() < 20 || band.Dy() < 6 {
		return recoveryEmotionBandUnknown
	}

	// Sample both ends of the flat band, away from its centered cabin name.
	sideWidth := min(9, band.Dx()/4)
	channels := [3][]int{}
	for y := band.Min.Y + 2; y < band.Max.Y-2; y++ {
		for _, xRange := range [][2]int{
			{band.Min.X + 2, band.Min.X + sideWidth},
			{band.Max.X - sideWidth, band.Max.X - 2},
		} {
			for x := xRange[0]; x < xRange[1]; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				if a == 0 {
					continue
				}
				channels[0] = append(channels[0], int(r>>8))
				channels[1] = append(channels[1], int(g>>8))
				channels[2] = append(channels[2], int(b>>8))
			}
		}
	}
	if len(channels[0]) == 0 {
		return recoveryEmotionBandUnknown
	}

	medians := [3]int{}
	for channel := range channels {
		sort.Ints(channels[channel])
		medians[channel] = channels[channel][len(channels[channel])/2]
	}
	return classifyRecoveryEmotionBandColor(medians[0], medians[1], medians[2])
}

func recoveryEmotionOperatorCardSelected(img image.Image, card maa.Rect) bool {
	if img == nil || card[2] < 24 || card[3] < 24 {
		return false
	}
	marker := image.Rect(
		card[0]+card[2]-18,
		card[1]+1,
		card[0]+card[2]-1,
		card[1]+18,
	).Intersect(img.Bounds())
	if marker.Dx() < 8 || marker.Dy() < 8 {
		return false
	}
	channels := [3][]int{}
	for y := marker.Min.Y; y < marker.Max.Y; y++ {
		for x := marker.Min.X; x < marker.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a == 0 {
				continue
			}
			channels[0] = append(channels[0], int(r>>8))
			channels[1] = append(channels[1], int(g>>8))
			channels[2] = append(channels[2], int(b>>8))
		}
	}
	if len(channels[0]) == 0 {
		return false
	}
	medians := [3]int{}
	for channel := range channels {
		sort.Ints(channels[channel])
		medians[channel] = channels[channel][len(channels[channel])/2]
	}
	return classifyRecoveryEmotionBandColor(medians[0], medians[1], medians[2]) == recoveryEmotionBandManufacture
}

func classifyRecoveryEmotionBandColor(r, g, b int) recoveryEmotionBandColor {
	maximum := max(r, g, b)
	minimum := min(r, g, b)
	if maximum == 0 || float64(maximum-minimum)/float64(maximum) <= recoveryEmotionNeutralChromaRatio {
		return recoveryEmotionBandNeutral
	}

	// Compare normalized channel relationships instead of fixed RGB values.
	switch {
	case r > b && g > b && float64(recoveryEmotionAbs(r-g)) <= float64(maximum-minimum)*recoveryEmotionYellowChannelRatio:
		return recoveryEmotionBandManufacture
	case r > g && b > g:
		return recoveryEmotionBandReception
	case b > r && g > r:
		return recoveryEmotionBandControlNexus
	case g > r && r > b:
		return recoveryEmotionBandGrowth
	default:
		return recoveryEmotionBandUnknown
	}
}

func recoveryEmotionAbs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func recoveryEmotionOperatorResult(
	card recoveryEmotionOperatorCard,
	params recoveryEmotionOperatorRecognitionParam,
) *maa.CustomRecognitionResult {
	detail, _ := json.Marshal(map[string]any{
		"mode":         params.Mode,
		"target_index": params.TargetIndex,
		"row":          card.row + 1,
		"column":       card.column + 1,
	})
	return &maa.CustomRecognitionResult{Box: card.click, Detail: string(detail)}
}
