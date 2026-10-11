const assert = require('node:assert/strict')

// Fractional zoom rounds a 1px CSS border. Verify the painted selection stays
// inside its control instead of extending above the first clipped/scrolling row.
module.exports = async function assertSelectionOutline(page, selector, screenshot) {
    const originalZoom = await page.evaluate(() => document.documentElement.style.zoom)
    try {
        for (const zoom of [1, 1.25, 1.5, 1.75, 2]) {
            await page.evaluate(zoom => { document.documentElement.style.zoom = String(zoom) }, zoom)
            await page.waitForTimeout(500)
            const box = await page.locator(selector).evaluate(outline => {
                const control = outline.parentElement
                const outer = control.getBoundingClientRect()
                const inner = outline.getBoundingClientRect()
                return {
                    top: inner.top - outer.top, left: inner.left - outer.left,
                    bottom: outer.bottom - inner.bottom, right: outer.right - inner.right,
                    radius: parseFloat(getComputedStyle(outline).borderTopLeftRadius),
                }
            })
            for (const side of ['top', 'left', 'bottom', 'right']) {
                assert(box[side] >= -0.02, `${selector} ${side} must stay inside the control at ${zoom * 100}% zoom: ${JSON.stringify(box)}`)
            }
            assert(box.radius > 0, 'selection keeps its rounded corners')
            if (zoom === 1.5 && screenshot) await page.locator(selector).locator('..').screenshot({ path: screenshot })
        }
        // Keyboard focus also remains visible within a clipped first row.
        await page.keyboard.press('Tab')
        await page.locator(selector).evaluate(outline => outline.parentElement.focus())
        const focus = await page.locator(selector).evaluate(outline => {
            const control = outline.parentElement
            const style = getComputedStyle(control)
            return { visible: control.matches(':focus-visible'), width: parseFloat(style.outlineWidth), offset: parseFloat(style.outlineOffset) }
        })
        assert(focus.visible && focus.width > 0 && focus.width + focus.offset <= 0, 'keyboard focus is visible inside the control')
    } finally {
        await page.evaluate(zoom => { document.documentElement.style.zoom = zoom }, originalZoom)
        await page.waitForTimeout(500)
    }
}
