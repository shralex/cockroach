// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scbuildstmt

import (
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/errors"
	"github.com/lib/pq/oid"
)

// TODO: (fqazi) Move decomposition into its own package that will no longer
// take in any BuildCtx. Our goal will be to instead just generate the elements,
// and let the caller deal with them after in terms of adding or dropping.

// TODO: (fqazi) Once we get create operations working we should start adding
// unit tests to test conversions both ways:
// 1) Convert an existing object
// 2) Drop it
// 3) Recreate it with the new schema changer
// Next compare the contents of the new and old descriptors to validate the
// conversion process.

// addOrDropForDir enqueues for add or drop depending on the direction.
func addOrDropForDir(b BuildCtx, dir scpb.Target_Direction, elem scpb.Element) {
	if dir == scpb.Target_ADD {
		b.EnqueueAdd(elem)
	} else {
		b.EnqueueDrop(elem)
	}
}

// columnDescToElement converts an individual column descriptor
// into the equivalent column element.
func columnDescToElement(
	table catalog.TableDescriptor,
	column descpb.ColumnDescriptor,
	familyName *string,
	familyID *descpb.FamilyID,
) *scpb.Column {
	nilToEmptyString := func(val *string) string {
		if val == nil {
			return ""
		}
		return *val
	}
	// Find the column family entries, when they
	// aren't populated
	if familyID == nil &&
		!column.Virtual {
		columnFamilies := table.GetFamilies()
		familyIdx := -1
		for idx, family := range columnFamilies {
			for _, col := range family.ColumnIDs {
				if col == column.ID {
					familyIdx = idx
					break
				}
			}
			if familyIdx != -1 {
				break
			}
		}
		familyID = &columnFamilies[familyIdx].ID
		familyName = &columnFamilies[familyIdx].Name
	} else if column.Virtual {
		localID := descpb.FamilyID(0)
		localName := ""
		familyID = &localID
		familyName = &localName
	}

	return &scpb.Column{
		TableID:                           table.GetID(),
		ColumnID:                          column.ID,
		Type:                              column.Type,
		FamilyName:                        *familyName,
		FamilyID:                          *familyID,
		Nullable:                          column.Nullable,
		DefaultExpr:                       nilToEmptyString(column.DefaultExpr),
		OnUpdateExpr:                      nilToEmptyString(column.OnUpdateExpr),
		Hidden:                            column.Hidden,
		Inaccessible:                      column.Inaccessible,
		GeneratedAsIdentitySequenceOption: nilToEmptyString(column.GeneratedAsIdentitySequenceOption),
		GeneratedAsIdentityType:           column.GeneratedAsIdentityType,
		UsesSequenceIds:                   column.UsesSequenceIds,
		ComputerExpr:                      nilToEmptyString(column.ComputeExpr),
		PgAttributeNum:                    column.GetPGAttributeNum(),
		SystemColumnKind:                  column.SystemColumnKind,
		Virtual:                           column.Virtual,
	}
}

type exprType int

const (
	exprTypeDefault  exprType = 0
	exprTypeOnUpdate exprType = 1
	exprTypeComputed exprType = 2
	exprTypeCheck    exprType = 3
)

// decomposeExprToTypeRef takes table expressions for example default
// expressions or check constraint expressions, parses them and generates
// elements for tracking type references to user defined types within these
// expressions. The type reference elements will be used to mutate descriptor
// so that both forward and back references are added for user defined types
// This function can be used for both  column based references or check
// constraints, so the field expressionID will refer to either a column ID or
// check constraint ordinal.
func decomposeExprToElements(
	b BuildCtx,
	exprString string,
	exprType exprType,
	tableID descpb.ID,
	expressionID uint32,
	dir scpb.Target_Direction,
) {
	// Empty expressions don't have any type references.
	if exprString == "" {
		return
	}
	// Get all available type references and create nodes
	// for dropping these type references.
	visitor := &tree.TypeCollectorVisitor{
		OIDs: make(map[oid.Oid]struct{}),
	}
	// Deal with any default expressions with types.
	expr, err := parser.ParseExpr(exprString)
	if err != nil {
		panic(err)
	}
	tree.WalkExpr(visitor, expr)
	for oid := range visitor.OIDs {
		baseTypeID, err := typedesc.UserDefinedTypeOIDToID(oid)
		onErrPanic(err)
		baseTypeDesc := b.MustReadType(baseTypeID)
		typeClosure, err := baseTypeDesc.GetIDClosure()
		onErrPanic(err)
		for typeID := range typeClosure {
			var typeRef scpb.Element
			switch exprType {
			case exprTypeDefault:
				typeRef = &scpb.DefaultExprTypeReference{
					TableID:  tableID,
					ColumnID: descpb.ColumnID(expressionID),
					TypeID:   typeID,
				}
			case exprTypeComputed:
				typeRef = &scpb.ComputedExprTypeReference{
					TableID:  tableID,
					ColumnID: descpb.ColumnID(expressionID),
					TypeID:   typeID,
				}
			case exprTypeOnUpdate:
				typeRef = &scpb.OnUpdateExprTypeReference{
					TableID:  tableID,
					ColumnID: descpb.ColumnID(expressionID),
					TypeID:   typeID,
				}
			case exprTypeCheck:
				typeRef = &scpb.CheckConstraintTypeReference{
					TableID:           tableID,
					ConstraintOrdinal: expressionID,
					TypeID:            typeID,
				}
			}
			addOrDropForDir(b, dir, typeRef)
		}
	}
}

// decomposeDefaultExprToElements converts and inserts default
// expression elements into the graph.
func decomposeDefaultExprToElements(
	b BuildCtx, table catalog.TableDescriptor, column catalog.Column, dir scpb.Target_Direction,
) {
	if !column.HasDefault() || column.ColumnDesc().HasNullDefault() {
		return
	}
	defaultExpr := column.GetDefaultExpr()
	sequenceIDs := make([]descpb.ID, 0, column.NumUsesSequences())
	for idx := 0; idx < column.NumUsesSequences(); idx++ {
		sequenceIDs = append(sequenceIDs, column.GetUsesSequenceID(idx))
	}
	expressionElem := scpb.DefaultExpression{
		TableID:         table.GetID(),
		ColumnID:        column.GetID(),
		DefaultExpr:     defaultExpr,
		UsesSequenceIDs: sequenceIDs,
	}
	if !b.HasTarget(dir, &expressionElem) {
		addOrDropForDir(b, dir, &expressionElem)
		// Decompose any elements required for expressions.
		decomposeExprToElements(b,
			defaultExpr, exprTypeDefault, table.GetID(), uint32(column.GetID()), dir)
	}
}

// decomposeDescToElements converts generic parts
// of a descriptor into an elements in the graph.
func decomposeDescToElements(b BuildCtx, tbl catalog.Descriptor, dir scpb.Target_Direction) {
	// Decompose all security settings
	privileges := tbl.GetPrivileges()
	ownerElem := scpb.Owner{
		DescriptorID: tbl.GetID(),
		Owner:        privileges.Owner().Normalized(),
	}
	addOrDropForDir(b, dir, &ownerElem)

	for _, user := range privileges.Users {
		userElem := scpb.UserPrivileges{
			DescriptorID: tbl.GetID(),
			Username:     user.User().Normalized(),
			Privileges:   user.Privileges,
		}
		addOrDropForDir(b, dir, &userElem)
	}
}

func decomposeColumnIntoElements(
	b BuildCtx, tbl catalog.TableDescriptor, column catalog.Column, dir scpb.Target_Direction,
) {
	if column.IsHidden() {
		return
	}
	addOrDropForDir(b, dir,
		&scpb.ColumnName{
			TableID:  tbl.GetID(),
			ColumnID: column.GetID(),
			Name:     column.GetName(),
		},
	)
	addOrDropForDir(b, dir,
		columnDescToElement(tbl, column.ColumnDescDeepCopy(), nil, nil))
	// Add references for the column types.
	typeClosure, err := typedesc.GetTypeDescriptorClosure(column.GetType())
	onErrPanic(err)
	for typeID := range typeClosure {
		addOrDropForDir(b, dir,
			&scpb.ColumnTypeReference{
				TableID:  tbl.GetID(),
				ColumnID: column.GetID(),
				TypeID:   typeID,
			})
	}
	// Convert any default expressions.
	decomposeDefaultExprToElements(b, tbl, column, dir)
	// Deal with computed and on update expressions
	decomposeExprToElements(b,
		column.GetComputeExpr(),
		exprTypeComputed,
		tbl.GetID(),
		uint32(column.GetID()),
		dir)
	decomposeExprToElements(b,
		column.GetOnUpdateExpr(),
		exprTypeOnUpdate,
		tbl.GetID(),
		uint32(column.GetID()),
		dir)
	// If there was a sequence owner dependency clean that up next.
	if column.NumOwnsSequences() > 0 {
		// Drop the depends on within the sequence side.
		for seqOrd := 0; seqOrd < column.NumOwnsSequences(); seqOrd++ {
			seqID := column.GetOwnsSequenceID(seqOrd)
			// Remove dependencies to this sequences.
			sequenceOwnedBy := &scpb.SequenceOwnedBy{SequenceID: seqID,
				OwnerTableID: tbl.GetID()}
			addOrDropForDir(b, dir, sequenceOwnedBy)
		}
	}
	// If there was a sequence dependency track those.
	if column.NumUsesSequences() > 0 {
		// Drop the depends on within the sequence side.
		for seqOrd := 0; seqOrd < column.NumUsesSequences(); seqOrd++ {
			seqID := column.GetUsesSequenceID(seqOrd)
			// Remove dependencies to this sequences.
			relationDep := &scpb.RelationDependedOnBy{TableID: seqID,
				DependedOnBy: tbl.GetID(),
				ColumnID:     column.GetID()}
			if !b.HasTarget(dir, relationDep) {
				addOrDropForDir(b, dir, relationDep)
			}
		}
	}
}

// decomposeViewDescToElements converts view specific
// parts of a table descriptor into elements for the graph.
func decomposeViewDescToElements(
	b BuildCtx, view catalog.TableDescriptor, dir scpb.Target_Direction,
) {
	dependIDs := catalog.DescriptorIDSet{}
	for _, typeRef := range view.GetDependsOnTypes() {
		typeDesc := b.MustReadType(typeRef)
		typeClosure, err := typeDesc.GetIDClosure()
		onErrPanic(err)
		for typeID := range typeClosure {
			dependIDs.Add(typeID)
		}
	}
	for _, typeRef := range dependIDs.Ordered() {
		typeDep := &scpb.ViewDependsOnType{
			TableID: view.GetID(),
			TypeID:  typeRef,
		}
		addOrDropForDir(b, dir, typeDep)
	}
}

// decomposeSequenceDescToElements converts sequence specific
// parts of a table descriptor into elements for the graph.
func decomposeSequenceDescToElements(
	b BuildCtx, seq catalog.TableDescriptor, dir scpb.Target_Direction,
) {
	if seq.GetSequenceOpts().SequenceOwner.OwnerTableID != descpb.InvalidID {
		sequenceOwnedBy := &scpb.SequenceOwnedBy{
			SequenceID:   seq.GetID(),
			OwnerTableID: seq.GetSequenceOpts().SequenceOwner.OwnerTableID}
		if !b.HasTarget(dir, sequenceOwnedBy) {
			addOrDropForDir(b, dir, sequenceOwnedBy)
		}
	}
}

// decomposeTableDescToElements converts a table/view/sequence into
// parts of a table descriptor into elements for the graph.
func decomposeTableDescToElements(
	b BuildCtx, tbl catalog.TableDescriptor, dir scpb.Target_Direction,
) {
	var objectElem scpb.Element
	switch {
	case tbl.IsTable():
		objectElem = &scpb.Table{
			TableID: tbl.GetID(),
		}
	case tbl.IsView():
		objectElem = &scpb.View{
			TableID: tbl.GetID(),
		}
	case tbl.IsSequence():
		objectElem = &scpb.Sequence{
			SequenceID: tbl.GetID(),
		}
	default:
		panic(errors.Newf("unable to convert tbl descriptor that is not a tbl/view/sequence"))
	}
	// If the node is already added then skip
	// adding it again.
	if b.HasTarget(dir, objectElem) {
		return
	}
	addOrDropForDir(b, dir, objectElem)
	nameElem := scpb.Namespace{
		Name:         tbl.GetName(),
		DatabaseID:   tbl.GetParentID(),
		SchemaID:     tbl.GetParentSchemaID(),
		DescriptorID: tbl.GetID(),
	}
	addOrDropForDir(b, dir, &nameElem)
	// Convert common fields for descriptors into elements.
	decomposeDescToElements(b, tbl, dir)
	switch {
	case tbl.IsTable():
		// Decompose columns into elements.
		for _, column := range tbl.AllColumns() {
			decomposeColumnIntoElements(b, tbl, column, dir)
		}
		// Decompose indexes into elements.
		for _, index := range tbl.AllIndexes() {
			if index.Primary() {
				primaryIndex, indexName := primaryIndexElemFromDescriptor(index.IndexDesc(), tbl)
				addOrDropForDir(b, dir, primaryIndex)
				addOrDropForDir(b, dir, indexName)

			} else {
				secondaryIndex, indexName := secondaryIndexElemFromDescriptor(index.IndexDesc(), tbl)
				addOrDropForDir(b, dir, secondaryIndex)
				addOrDropForDir(b, dir, indexName)
			}
		}
	case tbl.IsSequence():
		decomposeSequenceDescToElements(b, tbl, dir)
	case tbl.IsView():
		decomposeViewDescToElements(b, tbl, dir)

	}
	// Go through outbound/inbound foreign keys
	err := tbl.ForeachOutboundFK(func(fk *descpb.ForeignKeyConstraint) error {
		outBoundFk := scpb.ForeignKey{
			OriginID:         fk.OriginTableID,
			OriginColumns:    fk.OriginColumnIDs,
			ReferenceColumns: fk.ReferencedColumnIDs,
			ReferenceID:      fk.ReferencedTableID,
			OnUpdate:         fk.OnUpdate,
			OnDelete:         fk.OnDelete,
			Name:             fk.Name,
		}
		if !b.HasTarget(dir, &outBoundFk) {
			addOrDropForDir(b, dir, &outBoundFk)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	err = tbl.ForeachInboundFK(func(fk *descpb.ForeignKeyConstraint) error {
		inBoundFk := scpb.ForeignKeyBackReference{
			OriginID:         fk.ReferencedTableID,
			OriginColumns:    fk.ReferencedColumnIDs,
			ReferenceID:      fk.OriginTableID,
			ReferenceColumns: fk.OriginColumnIDs,
			OnUpdate:         fk.OnUpdate,
			OnDelete:         fk.OnDelete,
			Name:             fk.Name,
		}
		if !b.HasTarget(dir, &inBoundFk) {
			addOrDropForDir(b, dir, &inBoundFk)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	// Add any constraints without indexes first.
	for idx, constraint := range tbl.AllActiveAndInactiveUniqueWithoutIndexConstraints() {
		constraintName := &scpb.ConstraintName{
			TableID:           tbl.GetID(),
			ConstraintType:    scpb.ConstraintType_UniqueWithoutIndex,
			ConstraintOrdinal: uint32(idx),
			Name:              constraint.Name,
		}
		uniqueWithoutConstraint := &scpb.UniqueConstraint{
			TableID:           tbl.GetID(),
			ConstraintType:    scpb.ConstraintType_UniqueWithoutIndex,
			ConstraintOrdinal: uint32(idx),
			IndexID:           0, // Invalid ID
			ColumnIDs:         constraint.ColumnIDs,
		}
		addOrDropForDir(b, dir, uniqueWithoutConstraint)
		addOrDropForDir(b, dir, constraintName)
	}
	// Add any check constraints next.
	for idx, constraint := range tbl.AllActiveAndInactiveChecks() {
		constraintName := &scpb.ConstraintName{
			TableID:           tbl.GetID(),
			ConstraintType:    scpb.ConstraintType_Check,
			ConstraintOrdinal: uint32(idx),
			Name:              constraint.Name,
		}
		checkConstraint := &scpb.CheckConstraint{
			ConstraintType:    scpb.ConstraintType_Check,
			ConstraintOrdinal: uint32(idx),
			TableID:           tbl.GetID(),
			Name:              constraint.Name,
			Validated:         constraint.Validity == descpb.ConstraintValidity_Validated,
			ColumnIDs:         constraint.ColumnIDs,
			Expr:              constraint.Expr,
		}
		decomposeExprToElements(b,
			constraint.Expr,
			exprTypeCheck,
			tbl.GetID(),
			uint32(idx),
			dir)
		addOrDropForDir(b, dir, checkConstraint)
		addOrDropForDir(b, dir, constraintName)
	}
	// Add locality information.
	addOrDropForDir(b, dir, &scpb.Locality{
		DescriptorID: tbl.GetID(),
		Locality:     tbl.GetLocalityConfig(),
	})
	// Inject any dependencies into the plan.
	for _, dep := range tbl.GetDependsOn() {
		dependsOn := &scpb.RelationDependedOnBy{
			DependedOnBy: tbl.GetID(),
			TableID:      dep,
		}
		if !b.HasTarget(dir, dependsOn) {
			addOrDropForDir(b, dir, dependsOn)
		}
	}
	for _, depBy := range tbl.GetDependedOnBy() {
		if len(depBy.ColumnIDs) == 0 {
			dependedOnBy := &scpb.RelationDependedOnBy{
				DependedOnBy: depBy.ID,
				TableID:      tbl.GetID(),
			}
			if !b.HasTarget(dir, dependedOnBy) {
				addOrDropForDir(b, dir, dependedOnBy)
			}
		}
		for _, colID := range depBy.ColumnIDs {
			dependedOnBy := &scpb.RelationDependedOnBy{
				DependedOnBy: depBy.ID,
				TableID:      tbl.GetID(),
				ColumnID:     colID,
			}
			if !b.HasTarget(dir, dependedOnBy) {
				addOrDropForDir(b, dir, dependedOnBy)
			}
		}
	}
	//TODO (fqazi) Computed Expressions / Update expressions can be moved out
	// of column (similar to the UML).
	//TODO (fqazi) Type references will later on need better handling.
}
