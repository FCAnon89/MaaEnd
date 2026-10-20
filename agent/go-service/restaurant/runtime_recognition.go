package restaurant

import (
	"crypto/sha256"
	"fmt"
	"image"
	"image/draw"
	"regexp"
	"slices"
	"strconv"

	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

const (
	runtimeStateAttach  = "state"
	runtimeStageKitchen = "kitchen"
	runtimeStageVerify  = "verify"
	runtimeStageRetry   = "retry"
)

var (
	tableCardLeft      = [6]int{188, 282, 377, 471, 566, 660}
	kitchenTabs        = [KitchenCount]maa.Rect{{174, 145, 209, 46}, {386, 145, 217, 46}, {605, 145, 220, 46}}
	kitchenHeadIcon    = maa.Rect{370, 504, 64, 64}
	runtimeRecipeCards = [6]maa.Rect{{488, 207, 301, 88}, {797, 207, 300, 88}, {488, 302, 301, 88}, {797, 302, 300, 88}, {488, 392, 301, 88}, {797, 392, 300, 88}}
)

type runtimeState struct {
	Ledger            Ledger                 `json:"ledger"`
	Stage             string                 `json:"stage"`
	DispatchFinishAt  int                    `json:"dispatch_finish_at_elapsed_seconds"`
	RetryAfterElapsed int                    `json:"retry_after_elapsed_seconds,omitempty"`
	Paused            bool                   `json:"paused"`
	PauseReason       string                 `json:"pause_reason,omitempty"`
	Icons             map[string]runtimeIcon `json:"icons,omitempty"`
}

type runtimeIcon struct {
	Fingerprint string `json:"fingerprint"`
	ItemID      string `json:"item_id"`
}

type kitchenQueueView struct {
	Count     int
	HeadID    string
	Remaining int
}

type RuntimeNextKitchenRecognition struct{}

var _ maa.CustomRecognitionRunner = (*RuntimeNextKitchenRecognition)(nil)

func (r *RuntimeNextKitchenRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil || arg.Img == nil {
		return nil, false
	}
	state, err := loadRuntimeState(ctx)
	if err != nil || state.Paused || state.Stage == runtimeStageVerify || state.Stage == runtimeStageRetry {
		return nil, false
	}
	if state.Stage == runtimeStageKitchen && state.Ledger.Pending != nil && detectSelectedKitchen(arg.Img) == state.Ledger.Pending.Kitchen {
		return nil, false
	}
	_, err = observeRuntimeFrame(ctx, arg.Img, &state, false)
	if err == nil && state.Ledger.Pending != nil {
		state.Stage = runtimeStageKitchen
	}
	if !storeRuntimeFrame(ctx, &state, err) || state.Ledger.Pending == nil {
		return nil, false
	}
	return runtimeKitchenTabResult(state.Ledger.Pending)
}

type RuntimeNextDishRecognition struct{}

var _ maa.CustomRecognitionRunner = (*RuntimeNextDishRecognition)(nil)

func (r *RuntimeNextDishRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil || arg.Img == nil {
		return nil, false
	}
	state, err := loadRuntimeState(ctx)
	if err != nil || state.Paused || (state.Stage != runtimeStageKitchen && state.Stage != runtimeStageRetry) || state.Ledger.Pending == nil {
		return nil, false
	}
	dispatch := *state.Ledger.Pending
	// Pipeline has clicked the tab. This node must wait for that selection;
	// a successful result always identifies the recipe that should be clicked.
	if detectSelectedKitchen(arg.Img) != dispatch.Kitchen {
		return nil, false
	}
	// A delayed addition from the previous click must be recorded before a retry.
	added, err := observeRuntimeFrame(ctx, arg.Img, &state, state.Stage == runtimeStageRetry)
	retryReady := true
	if err == nil && !added && state.Stage == runtimeStageRetry {
		retryReady = dispatchRetryReady(&state)
	}
	if !storeRuntimeFrame(ctx, &state, err) || added || !retryReady {
		return nil, false
	}
	if state.Ledger.counterCount() >= CounterTrayCount || len(state.Ledger.Kitchens[dispatch.Kitchen-1]) >= KitchenQueueSize {
		return nil, false
	}
	ids := make([]string, 0, len(state.Ledger.Plan.Dishes))
	for _, dish := range state.Ledger.Plan.Dishes {
		ids = append(ids, dish.DishID)
	}
	cards := runtimeRecipeCards
	for index, card := range cards {
		if state.Icons[fmt.Sprint(runtimeRecipeIcon(card))].ItemID == dispatch.DishID {
			cards[0], cards[index] = cards[index], cards[0]
			break
		}
	}
	for _, card := range cards {
		roi := runtimeRecipeIcon(card)
		id, err := matchRuntimeIcon(ctx, arg.Img, roi, ids, &state)
		if err != nil {
			storeRuntimeFrame(ctx, &state, err)
			return nil, false
		}
		if id != dispatch.DishID {
			continue
		}
		state.Stage = runtimeStageVerify
		state.RetryAfterElapsed = 0
		start := state.Ledger.Clock.ElapsedSeconds
		queue := state.Ledger.Kitchens[dispatch.Kitchen-1]
		if len(queue) > 0 {
			start = max(start, queue[len(queue)-1].FinishAt)
		}
		state.DispatchFinishAt = start + dispatch.Duration
		if !storeRuntimeFrame(ctx, &state, nil) {
			return nil, false
		}
		return &maa.CustomRecognitionResult{Box: roi, Detail: id}, true
	}
	storeRuntimeFrame(ctx, &state, fmt.Errorf("pending dish %q is not visible", dispatch.DishID))
	return nil, false
}

type RuntimeVerifyDispatchRecognition struct{}

var _ maa.CustomRecognitionRunner = (*RuntimeVerifyDispatchRecognition)(nil)

func (r *RuntimeVerifyDispatchRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil || arg.Img == nil {
		return nil, false
	}
	state, err := loadRuntimeState(ctx)
	if err != nil || state.Paused || state.Stage != runtimeStageVerify || state.Ledger.Pending == nil {
		return nil, false
	}
	dispatch := *state.Ledger.Pending
	added, err := observeRuntimeFrame(ctx, arg.Img, &state, true)
	if err == nil && !added {
		state.Stage = runtimeStageRetry
		dispatchRetryReady(&state)
		log.Info().Str("dish", dispatch.DishID).Int("kitchen", dispatch.Kitchen).
			Msg("restaurant is waiting for queue confirmation before retrying")
	}
	if !storeRuntimeFrame(ctx, &state, err) {
		return nil, false
	}
	return &maa.CustomRecognitionResult{Box: maa.Rect{210, 500, 230, 85}, Detail: dispatch.DishID}, true
}

func dispatchRetryReady(state *runtimeState) bool {
	if state.RetryAfterElapsed == 0 {
		state.RetryAfterElapsed = state.Ledger.Clock.ElapsedSeconds + 1
	}
	return state.Ledger.Clock.ElapsedSeconds >= state.RetryAfterElapsed
}

// RuntimeFailureRecognition routes an uncertain runtime ledger to FailedNode.
type RuntimeFailureRecognition struct{}

var _ maa.CustomRecognitionRunner = (*RuntimeFailureRecognition)(nil)

func (r *RuntimeFailureRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil {
		return nil, false
	}
	state, err := loadRuntimeState(ctx)
	if err != nil {
		return &maa.CustomRecognitionResult{Box: maa.Rect{165, 145, 660, 450}, Detail: err.Error()}, true
	}
	if !state.Paused {
		return nil, false
	}
	return &maa.CustomRecognitionResult{Box: maa.Rect{165, 145, 660, 450}, Detail: state.PauseReason}, true
}

func loadRuntimeState(ctx *maa.Context) (runtimeState, error) {
	state, found, err := readAttach[runtimeState](ctx, restaurantRuntimeNode, runtimeStateAttach)
	if err != nil {
		return runtimeState{}, err
	}
	if found {
		return state, nil
	}
	plan, found, err := readAttach[MenuPlan](ctx, restaurantPlanNode, menuPlanAttach)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("menu plan is not available")
		}
		return runtimeState{}, err
	}
	ledger, err := NewLedger(plan)
	if err != nil {
		return runtimeState{}, err
	}
	return runtimeState{Ledger: *ledger}, nil
}

func saveRuntimeState(ctx *maa.Context, state runtimeState) error {
	return writeAttach(ctx, restaurantRuntimeNode, runtimeStateAttach, state)
}

func runtimeKitchenTabResult(dispatch *Dispatch) (*maa.CustomRecognitionResult, bool) {
	if dispatch == nil || dispatch.Kitchen < 1 || dispatch.Kitchen > KitchenCount {
		return nil, false
	}
	return &maa.CustomRecognitionResult{
		Box:    kitchenTabs[dispatch.Kitchen-1],
		Detail: fmt.Sprintf("kitchen:%d", dispatch.Kitchen),
	}, true
}

func storeRuntimeFrame(ctx *maa.Context, state *runtimeState, frameErr error) bool {
	if frameErr != nil {
		if state.PauseReason != frameErr.Error() {
			log.Warn().Err(frameErr).Str("component", menuRecognitionComponent).Msg("restaurant dispatch is waiting for a consistent frame")
		}
		state.PauseReason = frameErr.Error()
	} else {
		state.PauseReason = ""
	}
	if err := saveRuntimeState(ctx, *state); err != nil {
		log.Error().Err(err).Str("component", menuRecognitionComponent).Msg("cannot save restaurant runtime state")
		return false
	}
	return frameErr == nil && !state.Paused
}

// Every dispatch stage reads the game clock and current queue; order scans may
// reuse the pending dispatch snapshot. OCR uncertainty waits for another frame.
func observeRuntimeFrame(ctx *maa.Context, img image.Image, state *runtimeState, verify bool) (bool, error) {
	snapshot, snapshotErr := captureGameSnapshot(ctx, img, state)
	if snapshotErr != nil && !snapshot.Known {
		return false, snapshotErr
	}
	kitchen := detectSelectedKitchen(img)
	if kitchen == 0 {
		return false, fmt.Errorf("selected kitchen is uncertain")
	}
	if verify && (state.Ledger.Pending == nil || state.Ledger.Pending.Kitchen != kitchen) {
		return false, fmt.Errorf("waiting for the dispatched kitchen")
	}
	view, err := readKitchenQueue(ctx, img, state, kitchen)
	if err != nil {
		return false, err
	}
	added, err := synchronizeRuntimeFrame(state, snapshot, kitchen, view, verify)
	if err != nil {
		return added, err
	}
	return added, snapshotErr
}

// Incomplete serving data can temporarily disagree with capacity. Only a complete
// scan can turn a ledger disagreement into a permanent pause.
func synchronizeRuntimeFrame(state *runtimeState, snapshot GameSnapshot, kitchen int, view kitchenQueueView, verify bool) (bool, error) {
	ledger := &state.Ledger
	clock := ledger.Clock
	elapsed, err := clock.Observe(snapshot.Remaining)
	if err != nil {
		return false, err
	}
	queue := ledger.Kitchens[kitchen-1]
	removed, added, err := queueProgress(queue, view, ledger.Pending, elapsed, verify)
	if err != nil {
		return false, err
	}
	if verify && !added && state.DispatchFinishAt > 0 && elapsed >= state.DispatchFinishAt {
		state.Paused = snapshot.Complete
		return false, fmt.Errorf("dispatch result was not observed before the dish could have finished; refusing to duplicate it")
	}
	if len(queue) > removed && (view.HeadID == "" || view.HeadID != queue[removed].DishID) {
		return false, fmt.Errorf("visible cooking head does not match kitchen %d FIFO", kitchen)
	}
	for index := 0; index < removed; index++ {
		queue[index].FinishAt = min(queue[index].FinishAt, elapsed)
	}
	calibrateQueue(queue[removed:], elapsed, view)
	if ledger.counterCount() >= CounterTrayCount {
		// advanceCooking will apply this already-observed full-counter interval
		// to every kitchen. Compensate this visible queue before that advance.
		for index := range queue {
			queue[index].FinishAt -= elapsed - ledger.AdvancedAt
		}
	}
	observation := ledger.Observe(snapshot)
	if observation.Paused {
		state.Paused = snapshot.Complete
		return false, fmt.Errorf("ledger cannot be reconciled: %s", observation.Reason)
	}
	if added {
		dispatch := *ledger.Pending
		if err := ledger.ConfirmDispatch(dispatch); err != nil {
			state.Paused = snapshot.Complete
			return false, err
		}
		state.Stage = ""
		state.RetryAfterElapsed = 0
	}
	// Another kitchen can have filled the counter during the same interval.
	// Its simulated pause is already included in the visible head countdown.
	calibrateQueue(ledger.Kitchens[kitchen-1], elapsed, view)
	expected := queueIDs(ledger.Kitchens[kitchen-1])
	if len(expected) != view.Count || (len(expected) > 0 && expected[0] != view.HeadID) {
		state.Paused = snapshot.Complete
		return false, fmt.Errorf("kitchen %d queue disagrees with the ledger: visible count=%d head=%q ledger=%v", kitchen, view.Count, view.HeadID, expected)
	}
	return added, nil
}

// Match only removal of a known FIFO prefix and, during verification, exactly
// one pending dish. An identical dish completing during the click can make
// both interpretations possible; wait until the game clock resolves it.
func queueProgress(queue []CookingJob, view kitchenQueueView, pending *Dispatch, elapsed int, verify bool) (int, bool, error) {
	matches := func(ids []string) bool {
		return len(ids) == view.Count && (len(ids) == 0 || ids[0] == view.HeadID)
	}
	without, with := -1, -1
	for removed := 0; removed <= len(queue); removed++ {
		expected := queueIDs(queue[removed:])
		if matches(expected) {
			without = removed
		}
		if verify && pending != nil && matches(append(expected, pending.DishID)) {
			with = removed
		}
	}
	if with >= 0 && without >= 0 {
		if !slices.Equal(queueIDs(queue[without:]), append(queueIDs(queue[with:]), pending.DishID)) {
			return 0, false, fmt.Errorf("queue count and head cannot distinguish the pending dish from a completed prefix")
		}
		if with > 0 && queue[with-1].FinishAt > elapsed {
			return 0, false, fmt.Errorf("queue change is ambiguous while a dish completes")
		}
		return with, true, nil
	}
	if with >= 0 {
		return with, true, nil
	}
	if without >= 0 {
		return without, false, nil
	}
	return 0, false, fmt.Errorf("visible queue count=%d head=%q does not match the recorded FIFO", view.Count, view.HeadID)
}

func calibrateQueue(queue []CookingJob, elapsed int, view kitchenQueueView) {
	if len(queue) == 0 || queue[0].DishID != view.HeadID {
		return
	}
	queue[0].FinishAt = elapsed + view.Remaining
	for index := 1; index < len(queue); index++ {
		queue[index].FinishAt = queue[index-1].FinishAt + queue[index].Duration
	}
}

func queueIDs(queue []CookingJob) []string {
	ids := make([]string, 0, len(queue))
	for _, job := range queue {
		ids = append(ids, job.DishID)
	}
	return ids
}

// Cache only identical icon pixels, including the matcher's surrounding search area.
// The current menu fixes each ROI's candidates for the lifetime of this runtime state.
func matchRuntimeIcon(ctx *maa.Context, img image.Image, roi maa.Rect, itemIDs []string, state *runtimeState) (string, error) {
	region := logicalRect(img, roi).Inset(-4).Intersect(img.Bounds())
	patch := image.NewRGBA(image.Rect(0, 0, region.Dx(), region.Dy()))
	draw.Draw(patch, patch.Bounds(), img, region.Min, draw.Src)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(patch.Pix))
	key := fmt.Sprint(roi)
	if cached, found := state.Icons[key]; found && cached.Fingerprint == fingerprint {
		return cached.ItemID, nil
	}
	id, err := matchIcon(ctx, img, roi, itemIDs)
	if err != nil {
		return "", err
	}
	if state.Icons == nil {
		state.Icons = make(map[string]runtimeIcon)
	}
	state.Icons[key] = runtimeIcon{Fingerprint: fingerprint, ItemID: id}
	return id, nil
}

func captureGameSnapshot(ctx *maa.Context, img image.Image, state *runtimeState) (GameSnapshot, error) {
	ledger := &state.Ledger
	timerTexts, err := recognizeOCR(ctx, img, maa.Rect{70, 12, 85, 27})
	if err != nil {
		return GameSnapshot{}, fmt.Errorf("read restaurant countdown: %w", err)
	}
	remaining := joinOCR(timerTexts)
	if _, err := ParseCountdown(remaining); err != nil {
		return GameSnapshot{}, err
	}
	if canReuseDispatchOrders(state) {
		// Confirm the current queue before rereading unrelated order cards.
		// Planning the next job and retrying still require a complete order scan.
		return dispatchQueueSnapshot(ledger, remaining), nil
	}
	itemIDs := make([]string, 0, len(ledger.MenuByID)+len(ledger.DrinkIDs))
	for _, dish := range ledger.Plan.Dishes {
		itemIDs = append(itemIDs, dish.DishID)
	}
	itemIDs = append(itemIDs, ledger.Plan.Drinks...)
	points := make([]OrderPoint, 0, 12)
	known := true
	var snapshotErr error
	for table := 1; table <= 6; table++ {
		tablePoints, err := readTablePoints(ctx, img, table, itemIDs, state)
		if err != nil {
			if snapshotErr == nil {
				snapshotErr = err
			}
			// A failed table must not hide another table's delivery or eating state.
			tablePoints = make([]OrderPoint, 2)
			for point := 1; point <= 2; point++ {
				previous, found := ledger.Points[orderPointStorageKey(OrderPointKey{Table: table, Point: point})]
				known = known && found
				tablePoints[point-1] = previous
			}
		}
		points = append(points, tablePoints...)
	}
	var needsRestock []string
	if snapshotErr == nil {
		needsRestock, snapshotErr = detectRestockMarkers(ctx, img, state)
	}
	return GameSnapshot{Remaining: remaining, Known: known, Complete: snapshotErr == nil, Points: points, NeedsRestock: needsRestock}, snapshotErr
}

func canReuseDispatchOrders(state *runtimeState) bool {
	if state.PauseReason != "" || state.Ledger.Pending == nil || (state.Stage != runtimeStageKitchen && state.Stage != runtimeStageVerify) {
		return false
	}
	// Include the pending portion, which may already be in the game's queue.
	portions := state.Ledger.counterCount() + 1
	for _, queue := range state.Ledger.Kitchens {
		portions += len(queue)
	}
	return portions < CounterTrayCount
}

func dispatchQueueSnapshot(ledger *Ledger, remaining string) GameSnapshot {
	points := make([]OrderPoint, 0, len(ledger.Points))
	for _, point := range ledger.Points {
		points = append(points, point)
	}
	return GameSnapshot{Remaining: remaining, Known: true, Complete: true, Points: points}
}

func readTablePoints(ctx *maa.Context, img image.Image, table int, itemIDs []string, state *runtimeState) ([]OrderPoint, error) {
	points := []OrderPoint{{Key: OrderPointKey{Table: table, Point: 1}}, {Key: OrderPointKey{Table: table, Point: 2}}}
	left := tableCardLeft[table-1]
	// Ordinary order cards have a white body. Empty tables, eating/ordering
	// status cards and both golden gourmand states do not expose dish points.
	if neutralWhiteRatio(img, maa.Rect{left + 1, 43, 3, 38}) < 0.9 || neutralWhiteRatio(img, maa.Rect{left + 84, 43, 3, 20}) < 0.9 {
		unservedDish := false
		for _, point := range points {
			previous := state.Ledger.Points[orderPointStorageKey(point.Key)]
			if _, dish := state.Ledger.MenuByID[previous.ItemID]; dish && previous.Exists && !previous.Served {
				unservedDish = true
				break
			}
		}
		if unservedDish {
			texts, err := recognizeOCR(ctx, img, maa.Rect{left, 68, 94, 26})
			if err != nil {
				return nil, err
			}
			if !ocrContainsAny(texts, "用餐中", "Dining", "食事中", "식사 중") {
				return nil, fmt.Errorf("table %d is waiting for delivery or eating confirmation", table)
			}
			// Eating confirms every item in this table's previous order was delivered.
			// Keep those IDs for one snapshot so the ledger consumes each only once.
			for index, point := range points {
				if previous := state.Ledger.Points[orderPointStorageKey(point.Key)]; previous.Exists {
					previous.Served = true
					points[index] = previous
				}
			}
		}
		return points, nil
	}
	pointCount := tablePointCount(img, table)
	if pointCount == 0 {
		return nil, fmt.Errorf("table %d point-count markers are uncertain", table)
	}
	for point := 1; point <= pointCount; point++ {
		id, err := matchRuntimeIcon(ctx, img, tablePointROI(table, point, pointCount), itemIDs, state)
		if err != nil {
			return nil, err
		}
		if id == "" {
			return nil, fmt.Errorf("table %d point %d has an unrecognized item", table, point)
		}
		points[point-1].ItemID = id
		points[point-1].Exists = true
		points[point-1].Served = servedMarkerPixelCount(img, tableServedMarkerROI(table, point, pointCount)) >= 5
	}
	return points, nil
}

// The status-bar layout determines the point count before matching icons.
// A missing icon in a two-point order must not turn it into a single order.
func tablePointCount(img image.Image, table int) int {
	left := tableCardLeft[table-1]
	if neutralWhiteRatio(img, maa.Rect{left + 40, 36, 11, 1}) >= 0.5 {
		return 0
	}
	leftBorder := neutralWhiteRatio(img, maa.Rect{left + 33, 32, 6, 6})
	rightBorder := neutralWhiteRatio(img, maa.Rect{left + 52, 32, 6, 6})
	if leftBorder >= 0.95 && rightBorder >= 0.95 {
		return 1
	}
	if leftBorder < 0.9 && rightBorder < 0.9 {
		return 2
	}
	return 0
}

func neutralWhiteRatio(img image.Image, roi maa.Rect) float64 {
	count := 0
	for y := roi[1]; y < roi[1]+roi[3]; y++ {
		for x := roi[0]; x < roi[0]+roi[2]; x++ {
			r, g, b, ok := logicalPixel(img, x, y)
			if ok && min(r, g, b) > 210 && max(r, g, b)-min(r, g, b) < 27 {
				count++
			}
		}
	}
	return float64(count) / float64(roi[2]*roi[3])
}

func tablePointROI(table, point, pointCount int) maa.Rect {
	offset := 8 + (point-1)*43
	if pointCount == 1 {
		offset = 29
	}
	return maa.Rect{tableCardLeft[table-1] + offset, 43, 33, 33}
}

func tableServedMarkerROI(table, point, pointCount int) maa.Rect {
	offset := 35 + (point-1)*11
	if pointCount == 1 {
		offset = 40
	}
	return maa.Rect{tableCardLeft[table-1] + offset, 33, 10, 3}
}

func servedMarkerPixelCount(img image.Image, rect maa.Rect) int {
	count := 0
	for y := rect[1]; y < rect[1]+rect[3]; y++ {
		for x := rect[0]; x < rect[0]+rect[2]; x++ {
			r, g, b, ok := logicalPixel(img, x, y)
			if ok && int(g) > 150 && int(g)-int(r) > 20 && int(g)-int(b) > 40 {
				count++
			}
		}
	}
	return count
}

func runtimeRecipeIcon(card maa.Rect) maa.Rect {
	return maa.Rect{card[0] + 31, card[1] + 13, 66, 66}
}

func detectRestockMarkers(ctx *maa.Context, img image.Image, state *runtimeState) ([]string, error) {
	plan := state.Ledger.Plan
	itemIDs := make([]string, 0, len(plan.Dishes))
	for _, dish := range plan.Dishes {
		itemIDs = append(itemIDs, dish.DishID)
	}
	marked := make([]string, 0)
	for _, card := range runtimeRecipeCards {
		if yellowPixelCount(img, maa.Rect{card[0] + 3, card[1] + 39, 24, 23}) < 20 {
			continue
		}
		itemID, err := matchRuntimeIcon(ctx, img, runtimeRecipeIcon(card), itemIDs, state)
		if err != nil {
			return nil, err
		}
		if itemID == "" {
			return nil, fmt.Errorf("supplement marker is visible on an unrecognized dish card")
		}
		marked = append(marked, itemID)
	}
	return marked, nil
}

var kitchenSecondsPattern = regexp.MustCompile(`(\d+)\s*[sS秒초]`)

func readKitchenQueue(ctx *maa.Context, img image.Image, state *runtimeState, kitchen int) (kitchenQueueView, error) {
	count, err := kitchenQueueCount(img, kitchen)
	if err != nil {
		return kitchenQueueView{}, err
	}
	view := kitchenQueueView{Count: count}
	if count == 0 {
		return view, nil
	}
	plan := state.Ledger.Plan
	ids := make([]string, 0, len(plan.Dishes))
	for _, dish := range plan.Dishes {
		ids = append(ids, dish.DishID)
	}
	// The right slot is the cooking head; waiting dishes use smaller icons.
	view.HeadID, err = matchRuntimeIcon(ctx, img, kitchenHeadIcon, ids, state)
	if err != nil {
		return view, err
	}
	if view.HeadID == "" {
		return view, fmt.Errorf("kitchen %d has %d occupied dots but its cooking head is unrecognized", kitchen, count)
	}
	texts, err := recognizeOCR(ctx, img, maa.Rect{kitchenHeadIcon[0], 553, 64, 22})
	if err != nil {
		return view, err
	}
	seconds := kitchenSecondsPattern.FindStringSubmatch(joinOCR(texts))
	if len(seconds) != 2 {
		return view, fmt.Errorf("cooking head countdown was not recognized")
	}
	view.Remaining, _ = strconv.Atoi(seconds[1])
	return view, nil
}

// Occupied dots fill from right to left. Require either green or neutral grey
// for every dot, so a failed colour read cannot masquerade as an empty queue.
func kitchenQueueCount(img image.Image, kitchen int) (int, error) {
	tab := kitchenTabs[kitchen-1]
	count := 0
	for dot := 0; dot < KitchenQueueSize; dot++ {
		cx, cy := tab[0]+tab[2]-27+dot*9, tab[1]+37
		green, grey := 0, 0
		for y := cy - 1; y <= cy+1; y++ {
			for x := cx - 1; x <= cx+1; x++ {
				r, g, b, ok := logicalPixel(img, x, y)
				if !ok {
					continue
				}
				if g > 140 && int(g)-int(r) > 60 && int(g)-int(b) > 30 {
					green++
				} else if min(r, g, b) > 100 && max(r, g, b)-min(r, g, b) < 25 {
					grey++
				}
			}
		}
		if green >= 5 {
			count++
		} else if grey < 5 || count > 0 {
			return 0, fmt.Errorf("kitchen %d queue dots are uncertain", kitchen)
		}
	}
	return count, nil
}

func detectSelectedKitchen(img image.Image) int {
	selected := 0
	bestRatio := 0.0
	for index, tab := range kitchenTabs {
		dark := 0
		total := 0
		region := maa.Rect{tab[0] + 6, tab[1] + 5, 85, 25}
		for y := region[1]; y < region[1]+region[3]; y++ {
			for x := region[0]; x < region[0]+region[2]; x++ {
				r, g, b, ok := logicalPixel(img, x, y)
				if !ok {
					continue
				}
				total++
				if int(r)+int(g)+int(b) < 270 {
					dark++
				}
			}
		}
		ratio := 0.0
		if total > 0 {
			ratio = float64(dark) / float64(total)
		}
		if ratio > bestRatio {
			bestRatio = ratio
			selected = index + 1
		}
	}
	if bestRatio < 0.35 {
		return 0
	}
	return selected
}
