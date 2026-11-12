/**
 * Reading a StatCard (components/kit/StatCard.tsx) the way a person does: the
 * card is a region named by its title, its first paragraph is the number, and
 * each fact is a list item "<value> <label>".
 */

import { screen } from "@testing-library/react"

/** The card's number as shown, or null when there is no such card. */
export function cardValue(title: string): string | null {
  const card = screen.queryByRole("region", { name: title })
  return card ? (card.querySelector("p")?.textContent ?? "").trim() : null
}

/** A fact's value as shown ("1,500"), or null when the card has no such fact. */
export function cardFact(title: string, label: string): string | null {
  const card = screen.queryByRole("region", { name: title })
  const item = card && Array.from(card.querySelectorAll("li")).find((li) => (li.textContent ?? "").trim().endsWith(` ${label}`))
  return item ? (item.textContent ?? "").trim().slice(0, -(label.length + 1)) : null
}
