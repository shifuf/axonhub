import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18next from 'i18next';
import { loadTS, readSource } from '../../../../tests/unit-loader.mjs';

const now = Date.parse('2026-10-25T04:00:00Z');
for (const language of ['en', 'zh-CN']) {
  test(`cooldown details distinguish remaining/until, expired and manual permanent in ${language}`, async () => {
    const i18n = i18next.createInstance();
    await i18n.init({ showSupportNotice: false, lng: language, keySeparator: false, resources: { [language]: { translation: JSON.parse(readSource(`locales/${language}/channels.json`)) } } });
    const { CooldownDetails } = loadTS('features/channels/components/cooldown-details.tsx', {
      'react-i18next': { useTranslation: () => ({ t: i18n.t.bind(i18n), i18n }) },
    });
    const render = (expiresAt) => renderToStaticMarkup(React.createElement(CooldownDetails, { reason: 'Rule 429 × 3', expiresAt, now }));
    const permanent = render(null);
    assert.match(permanent, /Rule 429 × 3/);
    assert.match(permanent, language === 'en' ? /Permanent.*manual/i : /永久.*手动/);
    assert.doesNotMatch(permanent, /Expired|已到期/);
    const timed = render('2026-10-25T04:47:00Z');
    assert.match(timed, /47/);
    assert.match(timed, language === 'en' ? /Until/ : /恢复时间/);
    assert.match(timed, /datetime="2026-10-25T04:47:00Z"/i);
    const expired = render('2026-10-24T00:00:00Z');
    assert.match(expired, language === 'en' ? /Expired/ : /已到期/);
    assert.doesNotMatch(expired, /Permanent|永久/);
  });
}
