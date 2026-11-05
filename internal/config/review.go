package config

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/cnlangzi/nightme/internal/yamlfile"
)

// reviewKey is the top-level mapping holding the /review settings.
const reviewKey = "review"

// reviewAgentKey is the key inside the review section that names
// the agent /review runs on.
const reviewAgentKey = "agent"

// SetReviewAgent writes name into the review.agent key of the
// config file at path. Every other node of the document — comments,
// sections and keys nightme does not model — is preserved.
//
// A missing file is created; a malformed file is an error and is
// left untouched.
func SetReviewAgent(path, name string) error {
	if path == "" {
		return errors.New("config: empty path")
	}

	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	root, err := yamlfile.Parse(data, path)
	if err != nil {
		return err
	}
	review, err := yamlfile.MapValueOrCreate(root, reviewKey)
	if err != nil {
		return err
	}
	yamlfile.SetMapValue(review, reviewAgentKey, name)

	out, err := yamlfile.Encode(root)
	if err != nil {
		return err
	}
	return yamlfile.WriteFileAtomic(path, out)
}

// UnsetReviewAgent removes the review.agent key from the config
// file at path, and the review section with it when it becomes
// empty. Other keys under review survive.
//
// A missing file is a no-op that reports success and creates
// nothing; a malformed file is an error and is left untouched.
func UnsetReviewAgent(path string) error {
	if path == "" {
		return errors.New("config: empty path")
	}

	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	root, err := yamlfile.Parse(data, path)
	if err != nil {
		return err
	}
	review := yamlfile.MapValue(root, reviewKey)
	if review == nil || review.Kind != yaml.MappingNode {
		return nil
	}

	changed := yamlfile.DeleteMapKey(review, reviewAgentKey)
	if len(review.Content) == 0 {
		changed = yamlfile.DeleteMapKey(root, reviewKey) || changed
	}
	if !changed {
		return nil
	}

	out, err := yamlfile.Encode(root)
	if err != nil {
		return err
	}
	return yamlfile.WriteFileAtomic(path, out)
}
