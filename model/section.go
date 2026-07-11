package model

import (
	"github.com/pkg/errors"
	"github.com/rs/xid"
)

type SectionID string

func NewSectionID() SectionID {
	return SectionID(xid.New().String())
}

type Section interface {
	ID() SectionID
	Branch() []SectionID
	Level() uint
	Document() Document
	Parent() Section
	Sections() []Section
	Start() int
	End() int
	Content() ([]byte, error)
}

func WalkSections(d interface{ Sections() []Section }, fn func(s Section) error) error {
	sections := d.Sections()
	for _, s := range sections {
		if err := fn(s); err != nil {
			return errors.WithStack(err)
		}
		if err := WalkSections(s, fn); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func CountSections(d interface{ Sections() []Section }) int {
	sections := d.Sections()
	total := len(sections)
	for _, s := range sections {
		total += CountSections(s)
	}
	return total
}
