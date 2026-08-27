// Browse page: anchor-scroll the year list to the selected day (falling back
// to today). A #day-... hash is already handled natively by the browser.
if (!location.hash) {
  const target =
    document.querySelector<HTMLElement>('.browse-day.selected') ??
    document.querySelector<HTMLElement>('.browse-day.today')
  target?.scrollIntoView({ block: 'start' })
}

export {}
