package graphqladapter

import (
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

type graphTypeRef struct {
	name    string
	element *graphTypeRef
	nonNull bool
}

func (r graphTypeRef) string() string {
	var value string
	if r.element != nil {
		value = "[" + r.element.string() + "]"
	} else {
		value = r.name
	}
	if r.nonNull {
		value += "!"
	}
	return value
}

func (r graphTypeRef) named() string {
	if r.element != nil {
		return r.element.named()
	}
	return r.name
}

type graphInput struct {
	name    string
	typeRef graphTypeRef
}

type graphField struct {
	name      string
	arguments []graphInput
	typeRef   graphTypeRef
}

type graphType struct {
	kind     schema.TypeKind
	name     string
	fields   []graphField
	values   []string
	variants []string
}

type schemaModel struct {
	roots         map[protocol.OperationKind]string
	types         map[string]*graphType
	explicitRoots bool
}

type schemaParser struct {
	lexer      *lexer
	limits     Limits
	model      schemaModel
	typeCount  int
	fieldCount int
}
