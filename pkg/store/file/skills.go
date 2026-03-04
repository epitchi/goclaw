package file

import (
	"github.com/epitchi/goclaw/pkg/skills"
	"github.com/epitchi/goclaw/pkg/store"
)

// FileSkillStore wraps skills.Loader to implement store.SkillStore.
type FileSkillStore struct {
	loader *skills.Loader
}

func NewFileSkillStore(loader *skills.Loader) *FileSkillStore {
	return &FileSkillStore{loader: loader}
}

// Loader returns the underlying skills.Loader for direct access during migration.
func (f *FileSkillStore) Loader() *skills.Loader { return f.loader }

func (f *FileSkillStore) ListSkills() []store.SkillInfo {
	items := f.loader.ListSkills()
	result := make([]store.SkillInfo, len(items))
	for i, item := range items {
		result[i] = skillInfoToStore(item)
	}
	return result
}

func (f *FileSkillStore) LoadSkill(name string) (string, bool) {
	return f.loader.LoadSkill(name)
}

func (f *FileSkillStore) LoadForContext(allowList []string) string {
	return f.loader.LoadForContext(allowList)
}

func (f *FileSkillStore) BuildSummary(allowList []string) string {
	return f.loader.BuildSummary(allowList)
}

func (f *FileSkillStore) GetSkill(name string) (*store.SkillInfo, bool) {
	info, ok := f.loader.GetSkill(name)
	if !ok {
		return nil, false
	}
	result := skillInfoToStore(*info)
	return &result, true
}

func (f *FileSkillStore) FilterSkills(allowList []string) []store.SkillInfo {
	items := f.loader.FilterSkills(allowList)
	result := make([]store.SkillInfo, len(items))
	for i, item := range items {
		result[i] = skillInfoToStore(item)
	}
	return result
}

func (f *FileSkillStore) Version() int64   { return f.loader.Version() }
func (f *FileSkillStore) BumpVersion()     { f.loader.BumpVersion() }
func (f *FileSkillStore) Dirs() []string   { return f.loader.Dirs() }

func skillInfoToStore(s skills.Info) store.SkillInfo {
	return store.SkillInfo{
		Name:        s.Name,
		Slug:        s.Slug,
		Path:        s.Path,
		BaseDir:     s.BaseDir,
		Source:      s.Source,
		Description: s.Description,
	}
}
