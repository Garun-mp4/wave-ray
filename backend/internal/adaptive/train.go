package adaptive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	maxTrainingFiles      = 128
	maxTrainingFileBytes  = 64 << 20
	maxTrainingTotalBytes = 256 << 20
	maxTrainingSamples    = 5_000
	maxClasses            = 16
	maxAmplitudeValues    = 4096
	maxJSONLineBytes      = 2 << 20
	trainingEpochs        = 200
	trainingBatchSize     = 32
)

type sample struct {
	features [FeatureCount]float64
	class    int
}

type TrainingDataset struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	SizeBytes int64  `json:"size_bytes"`
	Eligible  bool   `json:"eligible"`
	Reason    string `json:"reason,omitempty"`
}

type Evaluation struct {
	Accuracy float64 `json:"accuracy"`
	Samples  int     `json:"samples"`
	Correct  int     `json:"correct"`
}

type TrainingProgress struct {
	Epoch              int
	TotalEpochs        int
	ValidationAccuracy *float64
}

type TrainingOptions struct {
	Epochs       int
	BatchSize    int
	LearningRate float64
	WeightDecay  float64
	Patience     int
}

var safeDatasetID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Train fits the same 15-feature softmax classifier used by the Rust runtime.
// Input files are bounded synthetic or user-provided recordings named
// train_<class>_*.jsonl. The reported accuracy is in-sample only.
func Train(ctx context.Context, recordingsDir string) (*Model, error) {
	datasets, err := ListTrainingDatasets(recordingsDir)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(datasets))
	for _, dataset := range datasets {
		ids = append(ids, dataset.ID)
	}
	return TrainFiles(ctx, recordingsDir, ids, trainingEpochs, nil)
}

// ListTrainingDatasets returns only explicitly labelled training recordings.
// Dataset IDs are file stems, never caller-controlled paths.
func ListTrainingDatasets(recordingsDir string) ([]TrainingDataset, error) {
	root, err := checkedRecordingsDir(recordingsDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read recordings directory: %w", err)
	}
	datasets := make([]TrainingDataset, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "train_") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if !safeDatasetID.MatchString(id) {
			continue
		}
		label := classForFilename(name)
		if label == "" {
			continue
		}
		item := TrainingDataset{ID: id, Label: label}
		fileInfo, statErr := os.Lstat(filepath.Join(root, name))
		if statErr != nil {
			item.Reason = "file_could_not_be_inspected"
		} else if !fileInfo.Mode().IsRegular() {
			item.Reason = "not_a_regular_file"
		} else if fileInfo.Size() < 0 || fileInfo.Size() > maxTrainingFileBytes {
			item.SizeBytes = fileInfo.Size()
			item.Reason = "file_exceeds_size_limit"
		} else {
			item.SizeBytes = fileInfo.Size()
			item.Eligible = true
		}
		datasets = append(datasets, item)
	}
	sort.Slice(datasets, func(i, j int) bool { return datasets[i].ID < datasets[j].ID })
	return datasets, nil
}

// TrainFiles fits the adaptive-compatible activity model from the selected
// recording files. Progress is reported once per epoch; the callback must be
// fast and must not retain mutable training state.
func TrainFiles(ctx context.Context, recordingsDir string, datasetIDs []string, epochs int, progress func(TrainingProgress)) (*Model, error) {
	trained, _, err := TrainValidatedFiles(ctx, recordingsDir, datasetIDs, nil, TrainingOptions{
		Epochs: epochs, BatchSize: trainingBatchSize, LearningRate: 0.1,
	}, progress)
	return trained, err
}

// TrainValidatedFiles fits on the selected training recordings and, when
// supplied, evaluates the held-out files after each epoch for early stopping.
// Files remain the indivisible split unit so adjacent windows cannot cross it.
func TrainValidatedFiles(ctx context.Context, recordingsDir string, trainingIDs, validationIDs []string, options TrainingOptions, progress func(TrainingProgress)) (*Model, Evaluation, error) {
	if options.Epochs < 1 || options.Epochs > 1000 || options.BatchSize < 1 || options.BatchSize > 1024 || !finite(options.LearningRate) || options.LearningRate <= 0 || options.LearningRate > 1 || !finite(options.WeightDecay) || options.WeightDecay < 0 || options.WeightDecay > 1 || options.Patience < 0 || options.Patience > 1000 {
		return nil, Evaluation{}, errors.New("invalid model training configuration")
	}
	files, classNames, err := selectedFiles(recordingsDir, trainingIDs)
	if err != nil {
		return nil, Evaluation{}, err
	}
	if len(classNames) == 0 {
		return nil, Evaluation{}, errors.New("no labelled training datasets were selected")
	}
	samples := make([]sample, 0)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, Evaluation{}, err
		}
		loaded, loadErr := loadSamples(ctx, file.path, file.class, maxTrainingSamples-len(samples))
		if loadErr != nil {
			return nil, Evaluation{}, fmt.Errorf("read training recording %q: %w", filepath.Base(file.path), loadErr)
		}
		samples = append(samples, loaded...)
		if len(samples) > maxTrainingSamples {
			return nil, Evaluation{}, fmt.Errorf("training data exceeds %d samples", maxTrainingSamples)
		}
	}
	if len(samples) == 0 {
		return nil, Evaluation{}, errors.New("selected recordings contain no usable training samples")
	}
	validation := make([]sample, 0)
	if len(validationIDs) > 0 {
		validationFiles, _, selectErr := selectedFiles(recordingsDir, validationIDs)
		if selectErr != nil {
			return nil, Evaluation{}, selectErr
		}
		classIndex := make(map[string]int, len(classNames))
		for index, name := range classNames {
			classIndex[name] = index
		}
		for _, file := range validationFiles {
			index, exists := classIndex[file.label]
			if !exists {
				return nil, Evaluation{}, fmt.Errorf("validation recording %q has a class absent from training", filepath.Base(file.path))
			}
			loaded, loadErr := loadSamples(ctx, file.path, index, maxTrainingSamples-len(validation))
			if loadErr != nil {
				return nil, Evaluation{}, fmt.Errorf("read validation recording %q: %w", filepath.Base(file.path), loadErr)
			}
			validation = append(validation, loaded...)
			if len(validation) > maxTrainingSamples {
				return nil, Evaluation{}, fmt.Errorf("validation data exceeds %d samples", maxTrainingSamples)
			}
		}
		if len(validation) == 0 {
			return nil, Evaluation{}, errors.New("selected validation recordings contain no usable samples")
		}
	}
	trained, evaluation, err := fit(ctx, samples, validation, classNames, options, progress)
	if err != nil {
		return nil, Evaluation{}, err
	}
	if evaluation.Samples > 0 {
		trained.ValidationAccuracy = evaluation.Accuracy
		trained.ValidationSamples = evaluation.Samples
		trained.ValidationSplit = "recording_file_holdout"
	}
	return trained, evaluation, nil
}

// EvaluateFiles measures predictions on recordings excluded from TrainFiles.
// Callers choose and preserve the split; windows from one file are never split
// across the training and evaluation sets.
func EvaluateFiles(ctx context.Context, recordingsDir string, datasetIDs []string, model *Model) (Evaluation, error) {
	if err := model.Validate(); err != nil {
		return Evaluation{}, err
	}
	files, _, err := selectedFiles(recordingsDir, datasetIDs)
	if err != nil {
		return Evaluation{}, err
	}
	classIndexes := make(map[string]int, len(model.ClassNames))
	for index, name := range model.ClassNames {
		classIndexes[name] = index
	}
	result := Evaluation{}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return Evaluation{}, err
		}
		classIndex, ok := classIndexes[file.label]
		if !ok {
			return Evaluation{}, fmt.Errorf("validation recording %q has a class absent from training", filepath.Base(file.path))
		}
		loaded, loadErr := loadSamples(ctx, file.path, classIndex, maxTrainingSamples-result.Samples)
		if loadErr != nil {
			return Evaluation{}, fmt.Errorf("read validation recording %q: %w", filepath.Base(file.path), loadErr)
		}
		for _, item := range loaded {
			label, _, classifyErr := model.Classify(item.features)
			if classifyErr != nil {
				return Evaluation{}, classifyErr
			}
			if label == file.label {
				result.Correct++
			}
			result.Samples++
			if result.Samples > maxTrainingSamples {
				return Evaluation{}, fmt.Errorf("validation data exceeds %d samples", maxTrainingSamples)
			}
		}
	}
	if result.Samples == 0 {
		return Evaluation{}, errors.New("selected validation recordings contain no usable samples")
	}
	result.Accuracy = float64(result.Correct) / float64(result.Samples)
	return result, nil
}

type trainingFile struct {
	path  string
	class int
	label string
}

func selectedFiles(recordingsDir string, datasetIDs []string) ([]trainingFile, []string, error) {
	root, err := checkedRecordingsDir(recordingsDir)
	if err != nil {
		return nil, nil, err
	}
	if len(datasetIDs) == 0 {
		return nil, nil, errors.New("at least one dataset id is required")
	}
	if len(datasetIDs) > maxTrainingFiles {
		return nil, nil, fmt.Errorf("selected datasets exceed %d files", maxTrainingFiles)
	}
	seen := make(map[string]struct{}, len(datasetIDs))
	labelsByID := make(map[string]string, len(datasetIDs))
	pathsByID := make(map[string]string, len(datasetIDs))
	var totalBytes int64
	for _, id := range datasetIDs {
		if !safeDatasetID.MatchString(id) || !strings.HasPrefix(id, "train_") {
			return nil, nil, fmt.Errorf("invalid dataset id %q", id)
		}
		if _, ok := seen[id]; ok {
			return nil, nil, fmt.Errorf("duplicate dataset id %q", id)
		}
		seen[id] = struct{}{}
		name := id + ".jsonl"
		path := filepath.Join(root, name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return nil, nil, fmt.Errorf("inspect dataset %q: %w", id, statErr)
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxTrainingFileBytes {
			return nil, nil, fmt.Errorf("dataset %q is not a supported regular file or exceeds the size limit", id)
		}
		totalBytes += info.Size()
		if totalBytes > maxTrainingTotalBytes {
			return nil, nil, errors.New("selected recordings exceed the total size limit")
		}
		labelsByID[id] = classForFilename(name)
		pathsByID[id] = path
	}
	classNames := make([]string, 0)
	classIndexes := make(map[string]int)
	for _, id := range datasetIDs {
		label := labelsByID[id]
		if label == "" {
			return nil, nil, fmt.Errorf("dataset %q does not have a valid class label", id)
		}
		if _, ok := classIndexes[label]; !ok {
			if len(classNames) >= maxClasses {
				return nil, nil, fmt.Errorf("training data has more than %d classes", maxClasses)
			}
			classIndexes[label] = len(classNames)
			classNames = append(classNames, label)
		}
	}
	sort.Strings(classNames)
	classIndexes = make(map[string]int, len(classNames))
	for index, label := range classNames {
		classIndexes[label] = index
	}
	files := make([]trainingFile, 0, len(datasetIDs))
	for _, id := range datasetIDs {
		label := labelsByID[id]
		files = append(files, trainingFile{path: pathsByID[id], label: label, class: classIndexes[label]})
	}
	sort.Slice(files, func(i, j int) bool { return filepath.Base(files[i].path) < filepath.Base(files[j].path) })
	return files, classNames, nil
}

func checkedRecordingsDir(directory string) (string, error) {
	if strings.TrimSpace(directory) == "" {
		return "", errors.New("recordings directory is required")
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve recordings directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("inspect recordings directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("recordings directory must be a real directory")
	}
	return root, nil
}

func loadSamples(ctx context.Context, path string, classIndex, remaining int) ([]sample, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxTrainingFileBytes {
		return nil, errors.New("training recording is not a supported regular file or exceeds the size limit")
	}
	limited := &io.LimitedReader{R: file, N: maxTrainingFileBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLineBytes)
	samples := make([]sample, 0)
	lastAmplitudes := make(map[uint64][]float64)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
		if lineCount%128 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		features, ok := samplesFromLine(line, lastAmplitudes)
		if !ok {
			continue
		}
		for _, vector := range features {
			samples = append(samples, sample{features: vector, class: classIndex})
			if len(samples) > remaining {
				return nil, errors.New("training sample limit exceeded")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("JSONL line exceeds %d bytes or cannot be read: %w", maxJSONLineBytes, err)
	}
	if limited.N == 0 {
		return nil, errors.New("training recording grew beyond the file size limit while being read")
	}
	return samples, nil
}

type recordingNode struct {
	NodeID    *uint64   `json:"node_id"`
	Amplitude []float64 `json:"amplitude"`
}

type recordingNodeFeature struct {
	NodeID   *uint64            `json:"node_id"`
	Stale    bool               `json:"stale"`
	Features map[string]float64 `json:"features"`
}

type recordingFrame struct {
	Features     map[string]float64 `json:"features"`
	Nodes        []recordingNode    `json:"nodes"`
	NodeFeatures json.RawMessage    `json:"node_features"`
}

func samplesFromLine(line []byte, lastAmplitudes map[uint64][]float64) ([][FeatureCount]float64, bool) {
	var frame recordingFrame
	if err := json.Unmarshal(line, &frame); err != nil {
		return nil, false
	}
	if len(frame.NodeFeatures) > 0 && !bytes.Equal(bytes.TrimSpace(frame.NodeFeatures), []byte("null")) {
		var nodeFeatures []recordingNodeFeature
		if json.Unmarshal(frame.NodeFeatures, &nodeFeatures) != nil {
			return nil, false
		}
		vectors := make([][FeatureCount]float64, 0, len(nodeFeatures))
		for _, nodeFeature := range nodeFeatures {
			if nodeFeature.Stale || nodeFeature.NodeID == nil {
				continue
			}
			var amplitudes []float64
			for _, node := range frame.Nodes {
				if node.NodeID != nil && *node.NodeID == *nodeFeature.NodeID {
					amplitudes = node.Amplitude
					break
				}
			}
			if len(amplitudes) == 0 || len(amplitudes) > maxAmplitudeValues || sameAmplitudes(lastAmplitudes[*nodeFeature.NodeID], amplitudes) {
				continue
			}
			if !finiteSlice(amplitudes) {
				continue
			}
			lastAmplitudes[*nodeFeature.NodeID] = append([]float64(nil), amplitudes...)
			vectors = append(vectors, featuresFromMap(nodeFeature.Features, amplitudes))
		}
		return vectors, true
	}
	var amplitudes []float64
	if len(frame.Nodes) > 0 {
		amplitudes = frame.Nodes[0].Amplitude
	}
	if len(amplitudes) > maxAmplitudeValues || !finiteSlice(amplitudes) {
		return nil, false
	}
	return [][FeatureCount]float64{featuresFromMap(frame.Features, amplitudes)}, true
}

func featuresFromMap(values map[string]float64, amplitudes []float64) [FeatureCount]float64 {
	if len(amplitudes) > RecordedAmplitudeLimit {
		amplitudes = amplitudes[:RecordedAmplitudeLimit]
	}
	features := [FeatureCount]float64{
		values["variance"], values["motion_band_power"], values["breathing_band_power"],
		values["spectral_power"], values["dominant_freq_hz"], values["change_points"], values["mean_rssi"],
	}
	mean, std, skew, kurt, iqr, entropy, maximum, amplitudeRange := subcarrierStats(amplitudes)
	copy(features[7:], []float64{mean, std, skew, kurt, iqr, entropy, maximum, amplitudeRange})
	return features
}

func fit(ctx context.Context, samples, validation []sample, classNames []string, options TrainingOptions, progress func(TrainingProgress)) (*Model, Evaluation, error) {
	classCount := len(classNames)
	globalMean := [FeatureCount]float64{}
	classCounts := make([]int, classCount)
	classSums := make([][FeatureCount]float64, classCount)
	classSquares := make([][FeatureCount]float64, classCount)
	for _, item := range samples {
		if item.class < 0 || item.class >= classCount {
			return nil, Evaluation{}, errors.New("invalid training class index")
		}
		classCounts[item.class]++
		for i, value := range item.features {
			if !finite(value) {
				return nil, Evaluation{}, errors.New("training data contains a non-finite feature")
			}
			globalMean[i] += value
			classSums[item.class][i] += value
			classSquares[item.class][i] += value * value
		}
	}
	for i := range globalMean {
		globalMean[i] /= float64(len(samples))
	}
	globalStd := [FeatureCount]float64{}
	for _, item := range samples {
		for i, value := range item.features {
			delta := value - globalMean[i]
			globalStd[i] += delta * delta
		}
	}
	for i := range globalStd {
		globalStd[i] = math.Max(math.Sqrt(globalStd[i]/float64(len(samples))), 1e-9)
	}
	stats := make([]ClassStats, classCount)
	for class := range classNames {
		stats[class].Label = classNames[class]
		stats[class].Count = classCounts[class]
		count := math.Max(1, float64(classCounts[class]))
		for feature := 0; feature < FeatureCount; feature++ {
			mean := classSums[class][feature] / count
			variance := math.Max(0, classSquares[class][feature]/count-mean*mean)
			stats[class].Mean[feature] = mean
			stats[class].Stddev[feature] = math.Sqrt(variance)
		}
	}
	normalized := make([]sample, len(samples))
	for i, item := range samples {
		normalized[i].class = item.class
		for feature, value := range item.features {
			normalized[i].features[feature] = (value - globalMean[feature]) / (globalStd[feature] + 1e-9)
		}
	}
	normalizedValidation := make([]sample, len(validation))
	for i, item := range validation {
		if item.class < 0 || item.class >= classCount {
			return nil, Evaluation{}, errors.New("invalid validation class index")
		}
		normalizedValidation[i].class = item.class
		for feature, value := range item.features {
			if !finite(value) {
				return nil, Evaluation{}, errors.New("validation data contains a non-finite feature")
			}
			normalizedValidation[i].features[feature] = (value - globalMean[feature]) / (globalStd[feature] + 1e-9)
		}
	}
	weights := make([][]float64, classCount)
	for class := range weights {
		weights[class] = make([]float64, FeatureCount+1)
	}
	rngState := uint64(42)
	nextRandom := func() uint64 {
		rngState = rngState*6364136223846793005 + 1442695040888963407
		return rngState >> 33
	}
	bestValidationAccuracy := -1.0
	var bestWeights [][]float64
	epochsWithoutImprovement := 0
	completedEpochs := 0
	for epoch := 0; epoch < options.Epochs; epoch++ {
		if err := ctx.Err(); err != nil {
			return nil, Evaluation{}, err
		}
		for i := len(normalized) - 1; i > 0; i-- {
			j := int(nextRandom() % uint64(i+1))
			normalized[i], normalized[j] = normalized[j], normalized[i]
		}
		learningRate := options.LearningRate * (1 - float64(epoch)/float64(options.Epochs))
		for batchStart := 0; batchStart < len(normalized); batchStart += options.BatchSize {
			batchEnd := min(batchStart+options.BatchSize, len(normalized))
			gradient := make([][]float64, classCount)
			for class := range gradient {
				gradient[class] = make([]float64, FeatureCount+1)
			}
			batch := normalized[batchStart:batchEnd]
			for _, item := range batch {
				probabilities := softmaxLogits(weights, item.features)
				for class, probability := range probabilities {
					delta := probability
					if class == item.class {
						delta--
					}
					for feature, value := range item.features {
						gradient[class][feature] += delta * value
					}
					gradient[class][FeatureCount] += delta
				}
			}
			for class := range weights {
				for feature := range weights[class] {
					regularization := 0.0
					if feature < FeatureCount {
						regularization = options.WeightDecay * weights[class][feature]
					}
					weights[class][feature] -= learningRate * (gradient[class][feature]/float64(len(batch)) + regularization)
				}
			}
		}
		completedEpochs = epoch + 1
		var validationAccuracy *float64
		if len(normalizedValidation) > 0 {
			evaluation := evaluateWeights(weights, normalizedValidation)
			validationAccuracy = &evaluation.Accuracy
			if evaluation.Accuracy > bestValidationAccuracy {
				bestValidationAccuracy = evaluation.Accuracy
				bestWeights = cloneWeights(weights)
				epochsWithoutImprovement = 0
			} else {
				epochsWithoutImprovement++
			}
		}
		if progress != nil {
			progress(TrainingProgress{Epoch: epoch + 1, TotalEpochs: options.Epochs, ValidationAccuracy: validationAccuracy})
		}
		if len(normalizedValidation) > 0 && options.Patience > 0 && epochsWithoutImprovement >= options.Patience {
			break
		}
	}
	if completedEpochs == 0 {
		return nil, Evaluation{}, errors.New("training completed no epochs")
	}
	if len(bestWeights) > 0 {
		weights = bestWeights
	}
	correct := 0
	for _, item := range normalized {
		if argmax(softmaxLogits(weights, item.features)) == item.class {
			correct++
		}
	}
	model := &Model{
		ClassStats: stats, Weights: weights, GlobalMean: globalMean, GlobalStd: globalStd,
		TrainedFrames: len(samples), TrainingAccuracy: float64(correct) / float64(len(samples)),
		Version: TrainedModelVersion, ClassNames: append([]string(nil), classNames...),
	}
	if err := model.Validate(); err != nil {
		return nil, Evaluation{}, err
	}
	evaluation := evaluateWeights(weights, normalizedValidation)
	return model, evaluation, nil
}

func evaluateWeights(weights [][]float64, samples []sample) Evaluation {
	result := Evaluation{Samples: len(samples)}
	if len(samples) == 0 {
		return result
	}
	for _, item := range samples {
		if argmax(softmaxLogits(weights, item.features)) == item.class {
			result.Correct++
		}
	}
	result.Accuracy = float64(result.Correct) / float64(result.Samples)
	return result
}

func cloneWeights(weights [][]float64) [][]float64 {
	copyOfWeights := make([][]float64, len(weights))
	for index := range weights {
		copyOfWeights[index] = append([]float64(nil), weights[index]...)
	}
	return copyOfWeights
}

func softmaxLogits(weights [][]float64, features [FeatureCount]float64) []float64 {
	logits := make([]float64, len(weights))
	maxLogit := math.Inf(-1)
	for class, vector := range weights {
		logit := vector[FeatureCount]
		for feature, value := range features {
			logit += vector[feature] * value
		}
		logits[class] = logit
		maxLogit = math.Max(maxLogit, logit)
	}
	total := 0.0
	for class, value := range logits {
		logits[class] = math.Exp(value - maxLogit)
		total += logits[class]
	}
	for class := range logits {
		logits[class] /= total
	}
	return logits
}

func argmax(values []float64) int {
	best := 0
	for i := 1; i < len(values); i++ {
		if values[i] > values[best] {
			best = i
		}
	}
	return best
}

func classForFilename(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "empty"), strings.Contains(lower, "absent"):
		return "absent"
	case strings.Contains(lower, "still"), strings.Contains(lower, "sitting"), strings.Contains(lower, "standing"):
		return "present_still"
	case strings.Contains(lower, "walking"), strings.Contains(lower, "moving"):
		return "present_moving"
	case strings.Contains(lower, "active"), strings.Contains(lower, "exercise"), strings.Contains(lower, "running"):
		return "active"
	default:
		stem := strings.TrimSuffix(strings.TrimPrefix(lower, "train_"), ".jsonl")
		className := strings.SplitN(stem, "_", 2)[0]
		if len(className) > 64 {
			return ""
		}
		return className
	}
}

func finiteSlice(values []float64) bool {
	for _, value := range values {
		if !finite(value) {
			return false
		}
	}
	return true
}

func sameAmplitudes(left, right []float64) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
