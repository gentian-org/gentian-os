/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package homesync

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-vcard"
)

// An entry of the home and a record of an app meet in Values: the parts of an
// entry (Field), each as text. A time is RFC 3339 in UTC, a date is
// YYYY-MM-DD, AllDay is "true" or "false". A part that is absent is not
// known; one that is "" was emptied.
type Values map[Field]string

const (
	timeLayout = "2006-01-02T15:04:05Z"
	dateLayout = "2006-01-02"
	prodID     = "-//Gentian OS//home sync//EN"
)

// errNotAnEntry is a body of the home that holds no entry the service reads:
// a calendar object with no event, or nothing it can parse.
var errNotAnEntry = errors.New("not an entry of this kind")

// Read is the parts of a home entry, and its UID.
func Read(kind Kind, body []byte) (Values, string, error) {
	if kind == Contacts {
		return readCard(body)
	}
	return readEvent(body)
}

// Write applies the parts to a home entry and returns the entry: onto body
// where there is one, keeping everything the parts do not name, or as a new
// entry of the UID.
func Write(kind Kind, body []byte, uid string, v Values) ([]byte, error) {
	if kind == Contacts {
		return writeCard(body, uid, v)
	}
	return writeEvent(body, uid, v)
}

// master is the event of a calendar object the service keeps: the one that is
// no exception to a recurring one.
func master(cal *ical.Calendar) *ical.Component {
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent && c.Props.Get(ical.PropRecurrenceID) == nil {
			return c
		}
	}
	return nil
}

func readEvent(body []byte) (Values, string, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(body)).Decode()
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errNotAnEntry, err)
	}
	ev := master(cal)
	if ev == nil {
		return nil, "", errNotAnEntry
	}
	v := Values{
		Summary:     text(ev, ical.PropSummary),
		Description: text(ev, ical.PropDescription),
		Location:    text(ev, ical.PropLocation),
		RRule:       text(ev, ical.PropRecurrenceRule),
	}
	start := ev.Props.Get(ical.PropDateTimeStart)
	if start == nil {
		return nil, "", errNotAnEntry
	}
	if start.ValueType() == ical.ValueDate {
		day, err := start.DateTime(time.UTC)
		if err != nil {
			return nil, "", err
		}
		last := day
		if end := ev.Props.Get(ical.PropDateTimeEnd); end != nil {
			if e, err := end.DateTime(time.UTC); err == nil && e.After(day) {
				// DTEND of an all-day event is the day after its last.
				last = e.AddDate(0, 0, -1)
			}
		}
		v[AllDay], v[StartDate], v[EndDate] = "true", day.Format(dateLayout), last.Format(dateLayout)
		return v, text(ev, ical.PropUID), nil
	}
	s, err := start.DateTime(time.UTC)
	if err != nil {
		return nil, "", err
	}
	e := s.Add(time.Hour)
	if end := ev.Props.Get(ical.PropDateTimeEnd); end != nil {
		if t, err := end.DateTime(time.UTC); err == nil {
			e = t
		}
	} else if d := ev.Props.Get(ical.PropDuration); d != nil {
		if dur, err := d.Duration(); err == nil {
			e = s.Add(dur)
		}
	}
	v[AllDay], v[Start], v[End] = "false", s.UTC().Format(timeLayout), e.UTC().Format(timeLayout)
	return v, text(ev, ical.PropUID), nil
}

func text(c *ical.Component, name string) string {
	if p := c.Props.Get(name); p != nil {
		return p.Value
	}
	return ""
}

func writeEvent(body []byte, uid string, v Values) ([]byte, error) {
	var cal *ical.Calendar
	if len(body) > 0 {
		decoded, err := ical.NewDecoder(bytes.NewReader(body)).Decode()
		if err != nil {
			return nil, err
		}
		cal = decoded
	} else {
		cal = ical.NewCalendar()
		cal.Props.SetText(ical.PropVersion, "2.0")
		cal.Props.SetText(ical.PropProductID, prodID)
		ev := ical.NewComponent(ical.CompEvent)
		ev.Props.SetText(ical.PropUID, uid)
		cal.Children = append(cal.Children, ev)
	}
	ev := master(cal)
	if ev == nil {
		return nil, errNotAnEntry
	}
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	setText(ev, ical.PropSummary, v, Summary)
	setText(ev, ical.PropDescription, v, Description)
	setText(ev, ical.PropLocation, v, Location)
	if r, ok := v[RRule]; ok {
		if r == "" {
			ev.Props.Del(ical.PropRecurrenceRule)
		} else {
			p := ical.NewProp(ical.PropRecurrenceRule)
			p.SetValueType(ical.ValueRecurrence)
			p.Value = strings.TrimPrefix(r, "RRULE:")
			ev.Props.Set(p)
		}
	}
	if v[AllDay] == "true" {
		day, err := time.Parse(dateLayout, v[StartDate])
		if err != nil {
			return nil, fmt.Errorf("an all-day entry needs its first day: %w", err)
		}
		last := day
		if l, err := time.Parse(dateLayout, v[EndDate]); err == nil && !l.Before(day) {
			last = l
		}
		ev.Props.SetDate(ical.PropDateTimeStart, day)
		ev.Props.SetDate(ical.PropDateTimeEnd, last.AddDate(0, 0, 1))
		ev.Props.Del(ical.PropDuration)
	} else if s, ok := v[Start]; ok {
		start, err := time.Parse(timeLayout, s)
		if err != nil {
			return nil, fmt.Errorf("an entry needs its start: %w", err)
		}
		end := start.Add(time.Hour)
		if e, err := time.Parse(timeLayout, v[End]); err == nil && e.After(start) {
			end = e
		}
		ev.Props.SetDateTime(ical.PropDateTimeStart, start.UTC())
		ev.Props.SetDateTime(ical.PropDateTimeEnd, end.UTC())
		ev.Props.Del(ical.PropDuration)
	}
	var out bytes.Buffer
	if err := ical.NewEncoder(&out).Encode(cal); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func setText(c *ical.Component, name string, v Values, f Field) {
	s, ok := v[f]
	if !ok {
		return
	}
	if s == "" {
		c.Props.Del(name)
		return
	}
	c.Props.SetText(name, s)
}

func readCard(body []byte) (Values, string, error) {
	card, err := vcard.NewDecoder(bytes.NewReader(body)).Decode()
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errNotAnEntry, err)
	}
	v := Values{
		Name:         card.Value(vcard.FieldFormattedName),
		Email:        card.Value(vcard.FieldEmail),
		Title:        card.Value(vcard.FieldTitle),
		Note:         card.Value(vcard.FieldNote),
		Website:      card.Value(vcard.FieldURL),
		Organization: strings.Split(card.Value(vcard.FieldOrganization), ";")[0],
		Phone:        "",
		Mobile:       "",
	}
	for _, tel := range card[vcard.FieldTelephone] {
		if isCell(tel) {
			if v[Mobile] == "" {
				v[Mobile] = tel.Value
			}
		} else if v[Phone] == "" {
			v[Phone] = tel.Value
		}
	}
	if a := card.Address(); a != nil {
		v[Street], v[City], v[Zip], v[Country] = a.StreetAddress, a.Locality, a.PostalCode, a.Country
	} else {
		v[Street], v[City], v[Zip], v[Country] = "", "", "", ""
	}
	return v, card.Value(vcard.FieldUID), nil
}

func writeCard(body []byte, uid string, v Values) ([]byte, error) {
	card := vcard.Card{}
	if len(body) > 0 {
		decoded, err := vcard.NewDecoder(bytes.NewReader(body)).Decode()
		if err != nil {
			return nil, err
		}
		card = decoded
	} else {
		card.SetValue(vcard.FieldVersion, "3.0")
		card.SetValue(vcard.FieldUID, uid)
	}
	setCard(card, vcard.FieldFormattedName, v, Name)
	if n, ok := v[Name]; ok && n != "" && card.Name() == nil {
		card.SetName(&vcard.Name{GivenName: n})
	}
	setCard(card, vcard.FieldEmail, v, Email)
	setCard(card, vcard.FieldTitle, v, Title)
	setCard(card, vcard.FieldNote, v, Note)
	setCard(card, vcard.FieldURL, v, Website)
	setCard(card, vcard.FieldOrganization, v, Organization)
	if _, ok := v[Phone]; ok {
		setTel(card, v[Phone], false)
	}
	if _, ok := v[Mobile]; ok {
		setTel(card, v[Mobile], true)
	}
	if _, ok := v[Street]; ok {
		a := &vcard.Address{StreetAddress: v[Street], Locality: v[City], PostalCode: v[Zip], Country: v[Country]}
		delete(card, vcard.FieldAddress)
		if a.StreetAddress+a.Locality+a.PostalCode+a.Country != "" {
			card.AddAddress(a)
		}
	}
	var out bytes.Buffer
	if err := vcard.NewEncoder(&out).Encode(card); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func setCard(card vcard.Card, field string, v Values, f Field) {
	s, ok := v[f]
	if !ok {
		return
	}
	if s == "" {
		delete(card, field)
		return
	}
	card.SetValue(field, s)
}

// setTel replaces the card's first number of the one sort (a mobile one or
// any other) and keeps the rest.
func setTel(card vcard.Card, number string, mobile bool) {
	var kept []*vcard.Field
	replaced := false
	for _, f := range card[vcard.FieldTelephone] {
		if isCell(f) == mobile && !replaced {
			replaced = true
			continue
		}
		kept = append(kept, f)
	}
	if number != "" {
		f := &vcard.Field{Value: number, Params: vcard.Params{}}
		if mobile {
			f.Params.Add(vcard.ParamType, vcard.TypeCell)
		} else {
			f.Params.Add(vcard.ParamType, vcard.TypeVoice)
		}
		kept = append([]*vcard.Field{f}, kept...)
	}
	if len(kept) == 0 {
		delete(card, vcard.FieldTelephone)
		return
	}
	card[vcard.FieldTelephone] = kept
}

// isCell reports whether a number is a mobile one.
func isCell(f *vcard.Field) bool {
	for _, t := range f.Params.Types() {
		if strings.EqualFold(t, vcard.TypeCell) {
			return true
		}
	}
	return false
}
