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
	recoveryEmotionOperatorRecognitionName = "RecoveryEmotionCabinOperatorRecognition"
	recoveryEmotionCabinAttachNode         = "RecoveryEmotionSelectLowEmotion"
	recoveryEmotionOCRThreshold            = 0.15
	recoveryEmotionFirstBandOffset         = 77
	recoveryEmotionSecondBandOffset        = 217
	recoveryEmotionBandSearchMargin        = 60
	recoveryEmotionBandMinimumRun          = 14
	recoveryEmotionBandHeight              = 18
	recoveryEmotionLabelTextHeight         = 16
	recoveryEmotionNeutralChromaRatio      = 0.35
	recoveryEmotionYellowChannelRatio      = 0.2
	recoveryEmotionOperatorColumns         = 6
	recoveryEmotionOperatorColumnStep      = 90
	recoveryEmotionOperatorCardWidth       = 81
	recoveryEmotionOperatorCardHeight      = 115
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
	Mode string `json:"mode"`
}

type recoveryEmotionCabinAttach struct {
	Attach struct {
		CabinLabels    map[string][]string `json:"cabin_labels"`
		SelectedCabins map[string]bool     `json:"selected_cabins"`
	} `json:"attach"`
}

type recoveryEmotionCabinTarget struct {
	Labels        []string
	BandColor     recoveryEmotionBandColor
	Manufacturing bool
	Selected      bool
}

type recoveryEmotionOperatorCard struct {
	row    int
	column int
	label  maa.Rect
	band   maa.Rect
	card   maa.Rect
	click  maa.Rect
}

type recoveryEmotionOperatorRecognitionRunner struct{}

var _ maa.CustomRecognitionRunner = &recoveryEmotionOperatorRecognitionRunner{}

func (r *recoveryEmotionOperatorRecognitionRunner) Run(
	ctx *maa.Context,
	arg *maa.CustomRecognitionArg,
) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil || arg.Img == nil {
		return nil, false
	}
	if arg.Roi[2] <= 0 || arg.Roi[3] <= 0 {
		log.Error().Str("component", recoveryEmotionOperatorRecognitionName).Msg("recognition roi is empty")
		return nil, false
	}

	var params recoveryEmotionOperatorRecognitionParam
	if err := json.Unmarshal([]byte(arg.CustomRecognitionParam), &params); err != nil {
		log.Error().Err(err).Str("component", recoveryEmotionOperatorRecognitionName).Msg("failed to parse params")
		return nil, false
	}
	if params.Mode != "target" && params.Mode != "unassigned" {
		log.Error().Str("component", recoveryEmotionOperatorRecognitionName).Str("mode", params.Mode).Msg("unsupported scan mode")
		return nil, false
	}
	cabins, err := loadRecoveryEmotionCabinTargets(ctx)
	if err != nil {
		log.Error().Err(err).Str("component", recoveryEmotionOperatorRecognitionName).Msg("failed to load cabin labels")
		return nil, false
	}

	for _, card := range recoveryEmotionOperatorCards(arg.Img, arg.Roi) {
		bandColor := recoveryEmotionOperatorBandColor(arg.Img, card.band)
		texts, err := recognizeRecoveryEmotionCard(ctx, arg.Img, card.label)
		if err != nil {
			log.Debug().
				Err(err).
				Str("component", recoveryEmotionOperatorRecognitionName).
				Int("row", card.row+1).
				Int("column", card.column+1).
				Msg("card label OCR failed")
			texts = nil
		}
		log.Debug().
			Str("component", recoveryEmotionOperatorRecognitionName).
			Str("mode", params.Mode).
			Strs("ocr_texts", texts).
			Str("band_color", string(bandColor)).
			Int("band_y", card.band[1]).
			Int("row", card.row+1).
			Int("column", card.column+1).
			Msg("scanned recovery emotion operator card")

		if isRecoveryEmotionUnassigned(texts, bandColor, cabins) {
			log.Debug().
				Str("component", recoveryEmotionOperatorRecognitionName).
				Str("mode", params.Mode).
				Str("band_color", string(bandColor)).
				Int("row", card.row+1).
				Int("column", card.column+1).
				Msg("found first unassigned card")
			if params.Mode == "unassigned" {
				return recoveryEmotionOperatorResult(card, params), true
			}
			return nil, false
		}

		if params.Mode != "target" {
			continue
		}
		if !matchesRecoveryEmotionCabin(texts, bandColor, cabins) {
			continue
		}
		if recoveryEmotionOperatorCardSelected(arg.Img, card.card) {
			continue
		}
		log.Debug().
			Str("component", recoveryEmotionOperatorRecognitionName).
			Str("band_color", string(bandColor)).
			Int("row", card.row+1).
			Int("column", card.column+1).
			Msg("found matching cabin operator")
		return recoveryEmotionOperatorResult(card, params), true
	}

	log.Debug().
		Str("component", recoveryEmotionOperatorRecognitionName).
		Str("mode", params.Mode).
		Msg("no matching card found on current page")
	return nil, false
}

// recoveryEmotionOperatorCards builds visible row-major cards using each row's detected band offset.
func recoveryEmotionOperatorCards(img image.Image, roi maa.Rect) []recoveryEmotionOperatorCard {
	columnStep := roi[2] / recoveryEmotionOperatorColumns
	if columnStep <= 0 {
		return nil
	}
	scale := func(value int) int { return value * columnStep / recoveryEmotionOperatorColumnStep }
	cardWidth := scale(recoveryEmotionOperatorCardWidth)
	cardHeight := scale(recoveryEmotionOperatorCardHeight)
	bandHeight := recoveryEmotionBandHeight
	searchMargin := scale(recoveryEmotionBandSearchMargin)
	minimumRun := recoveryEmotionBandMinimumRun
	labelTextHeight := recoveryEmotionLabelTextHeight
	cardTopOffset := scale(71)
	bandRows := [...]int{
		roi[1] + scale(recoveryEmotionFirstBandOffset),
		roi[1] + scale(recoveryEmotionSecondBandOffset),
	}
	shifts := [2]int{}
	found := [2]bool{}
	for row, bandY := range bandRows {
		shifts[row], found[row] = recoveryEmotionOperatorBandShift(img, roi, bandY, cardWidth, bandHeight, searchMargin, minimumRun)
	}
	if !found[0] && found[1] {
		shifts[0] = shifts[1]
	} else if found[0] && !found[1] {
		shifts[1] = shifts[0]
	}

	cards := make([]recoveryEmotionOperatorCard, 0, len(bandRows)*recoveryEmotionOperatorColumns)
	for row, bandY := range bandRows {
		bandY += shifts[row]
		for column := 0; column < recoveryEmotionOperatorColumns; column++ {
			x := roi[0] + column*columnStep
			band := maa.Rect{x, bandY, cardWidth, bandHeight}
			label := maa.Rect{x, bandY, cardWidth, labelTextHeight}
			card := maa.Rect{x, bandY - cardTopOffset, cardWidth, cardHeight}
			visible := image.Rect(card[0], card[1], card[0]+card[2], card[1]+card[3]).Intersect(image.Rect(roi[0], roi[1], roi[0]+roi[2], roi[1]+roi[3]))
			visible = visible.Intersect(img.Bounds())
			if visible.Empty() {
				continue
			}
			clickX := x + cardWidth/2
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

// recoveryEmotionOperatorBandShift finds the vertical offset shared by colored bands in one row.
func recoveryEmotionOperatorBandShift(img image.Image, roi maa.Rect, expectedY, cardWidth, bandHeight, searchMargin, minimumRun int) (int, bool) {
	if roi[2] <= 0 || roi[3] < bandHeight {
		return 0, false
	}
	startY := max(roi[1], expectedY-searchMargin)
	endY := min(roi[1]+roi[3]-1, expectedY+searchMargin)
	if startY > endY {
		return 0, false
	}

	var shifts []int
	columnStep := roi[2] / recoveryEmotionOperatorColumns
	for column := 0; column < recoveryEmotionOperatorColumns; column++ {
		x := roi[0] + column*columnStep
		lastColor := recoveryEmotionBandUnknown
		runStart := startY
		runLength := 0
		flushRun := func() {
			if lastColor != recoveryEmotionBandUnknown && lastColor != recoveryEmotionBandNeutral && runLength >= minimumRun {
				bandY := runStart + (runLength-bandHeight)/2
				shifts = append(shifts, bandY-expectedY)
			}
			runLength = 0
		}
		for y := startY; y <= endY; y++ {
			color := recoveryEmotionOperatorBandColorAtY(img, x, y, cardWidth)
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

func recoveryEmotionOperatorBandColorAtY(img image.Image, x, y, cardWidth int) recoveryEmotionBandColor {
	return recoveryEmotionOperatorBandColor(img, maa.Rect{x, y, cardWidth, 1})
}

// loadRecoveryEmotionCabinTargets reads the canonical cabin labels and selected subset from the scan node.
func loadRecoveryEmotionCabinTargets(ctx *maa.Context) (map[string]recoveryEmotionCabinTarget, error) {
	raw, err := ctx.GetNodeJSON(recoveryEmotionCabinAttachNode)
	if err != nil {
		return nil, fmt.Errorf("get cabin attach from %s: %w", recoveryEmotionCabinAttachNode, err)
	}
	var node recoveryEmotionCabinAttach
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		return nil, fmt.Errorf("parse cabin attach from %s: %w", recoveryEmotionCabinAttachNode, err)
	}

	cabins := make(map[string]recoveryEmotionCabinTarget, len(node.Attach.CabinLabels))
	for key, labels := range node.Attach.CabinLabels {
		if len(labels) == 0 {
			continue
		}
		cabins[key] = recoveryEmotionCabinTarget{
			Labels:        labels,
			BandColor:     recoveryEmotionCabinBandColorForKey(key),
			Manufacturing: key == "manufacturing_i" || key == "manufacturing_ii",
			Selected:      node.Attach.SelectedCabins[key],
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
		if !cabin.Selected {
			continue
		}
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

// isRecoveryEmotionUnassigned lets an exact cabin name override a neutral color sample.
func isRecoveryEmotionUnassigned(
	texts []string,
	bandColor recoveryEmotionBandColor,
	cabins map[string]recoveryEmotionCabinTarget,
) bool {
	if matchesRecoveryEmotionLabel(texts, recoveryEmotionUnassignedLabels) {
		return true
	}
	if bandColor != recoveryEmotionBandNeutral {
		return false
	}
	for _, cabin := range cabins {
		if matchesRecoveryEmotionLabel(texts, cabin.Labels) {
			return false
		}
	}
	return true
}

// recognizeRecoveryEmotionCard reads only the cabin-name strip above the trust indicator.
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
	band := image.Rect(roi[0], roi[1], roi[0]+roi[2], roi[1]+roi[3]).Intersect(img.Bounds())
	if band.Dx() < 20 || band.Empty() {
		return recoveryEmotionBandUnknown
	}

	// Sample both ends of the flat band, away from its centered cabin name.
	sideWidth := band.Dx() * 9 / recoveryEmotionOperatorCardWidth
	horizontalInset := band.Dx() * 2 / recoveryEmotionOperatorCardWidth
	yStart := band.Min.Y
	yEnd := band.Max.Y
	if band.Dy() >= 6 {
		verticalInset := band.Dy() * 2 / recoveryEmotionBandHeight
		yStart += verticalInset
		yEnd -= verticalInset
	}
	return recoveryEmotionColorFromRegions(
		img,
		image.Rect(band.Min.X+horizontalInset, yStart, band.Min.X+sideWidth, yEnd),
		image.Rect(band.Max.X-sideWidth, yStart, band.Max.X-horizontalInset, yEnd),
	)
}

// recoveryEmotionColorFromRegions classifies the median color across clipped image regions.
func recoveryEmotionColorFromRegions(img image.Image, regions ...image.Rectangle) recoveryEmotionBandColor {
	channels := [3][]int{}
	for _, region := range regions {
		region = region.Intersect(img.Bounds())
		for y := region.Min.Y; y < region.Max.Y; y++ {
			for x := region.Min.X; x < region.Max.X; x++ {
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

// recoveryEmotionOperatorCardSelected reads the yellow check marker in the card's upper-right corner.
func recoveryEmotionOperatorCardSelected(img image.Image, card maa.Rect) bool {
	markerSize := card[2] * 17 / recoveryEmotionOperatorCardWidth
	markerInset := card[2] / recoveryEmotionOperatorCardWidth
	marker := image.Rect(
		card[0]+card[2]-markerSize-markerInset,
		card[1]+markerInset,
		card[0]+card[2]-markerInset,
		card[1]+markerInset+markerSize,
	).Intersect(img.Bounds())
	if marker.Dx() < 8 || marker.Dy() < 8 {
		return false
	}
	return recoveryEmotionColorFromRegions(img, marker) == recoveryEmotionBandManufacture
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
		"mode":   params.Mode,
		"row":    card.row + 1,
		"column": card.column + 1,
	})
	return &maa.CustomRecognitionResult{Box: card.click, Detail: string(detail)}
}
