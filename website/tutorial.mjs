// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import manifest from './tutorial.json' with { type: 'json' };

const extensions = { video: '.mp4', captions: '.vtt', subtitles: '.srt', chapters: '.vtt', poster: '.webp' };

export function validateTutorial(value) {
  if (value.repository !== 'microsoft/kognition' || !/^tutorial-\d{4}-\d{2}-\d{2}$/.test(value.releaseTag) ||
      typeof value.title !== 'string' || !value.title.trim() ||
      !Number.isFinite(value.durationSeconds) || value.durationSeconds <= 0 ||
      !value.assets || Object.keys(value.assets).length !== Object.keys(extensions).length ||
      !Array.isArray(value.chapters) || !value.chapters.length) {
    throw new Error('Invalid tutorial identity, duration or chapters.');
  }
  const names = new Set();
  for (const [role, extension] of Object.entries(extensions)) {
    const asset = value.assets?.[role];
    if (!asset || !/^[a-z0-9][a-z0-9.-]*$/.test(asset.name) || !asset.name.endsWith(extension) ||
        names.has(asset.name) || !Number.isSafeInteger(asset.bytes) || asset.bytes <= 0 ||
        !/^[a-f0-9]{64}$/.test(asset.sha256)) {
      throw new Error(`Invalid tutorial asset: ${role}`);
    }
    names.add(asset.name);
  }
  let previous = -1;
  for (const chapter of value.chapters) {
    if (!Number.isFinite(chapter.startSeconds) || chapter.startSeconds <= previous ||
        chapter.startSeconds >= value.durationSeconds || typeof chapter.title !== 'string' || !chapter.title.trim()) {
      throw new Error('Tutorial chapters must have titles and increasing times within the video.');
    }
    previous = chapter.startSeconds;
  }
  if (value.chapters[0].startSeconds !== 0) throw new Error('The first tutorial chapter must start at zero.');
  return value;
}

export const tutorial = manifest;
validateTutorial(tutorial);

export function tutorialPaths(base) {
  if (!base.startsWith('/') || !base.endsWith('/') || /[\\?#\s]/.test(base) ||
      base.includes('//') || base.split('/').some((part) => part === '.' || part === '..')) {
    throw new Error('Tutorial assets require a normalized absolute site base.');
  }
  return Object.fromEntries(Object.entries(tutorial.assets).map(([role, asset]) =>
    [role, `${base}media/tutorial/${tutorial.releaseTag}/${asset.name}`]));
}

export function formatTime(seconds) {
  const total = Math.floor(seconds);
  return `${String(Math.floor(total / 60)).padStart(2, '0')}:${String(total % 60).padStart(2, '0')}`;
}

function vttTime(seconds) {
  const milliseconds = Math.round(seconds * 1000);
  return `${String(Math.floor(milliseconds / 3600000)).padStart(2, '0')}:` +
    `${String(Math.floor(milliseconds / 60000) % 60).padStart(2, '0')}:` +
    `${String(Math.floor(milliseconds / 1000) % 60).padStart(2, '0')}.` +
    String(milliseconds % 1000).padStart(3, '0');
}

export function parseSubtitles(text, durationSeconds) {
  const blocks = text.replace(/^\uFEFF/, '').replace(/\r\n/g, '\n').trim().split(/\n{2,}/);
  let previousEnd = 0;
  return blocks.map((block, index) => {
    const match = /^(\d+)\n(\d{2}):(\d{2}):(\d{2}),(\d{3}) --> (\d{2}):(\d{2}):(\d{2}),(\d{3})\n([\s\S]+)$/.exec(block);
    if (!match || Number(match[1]) !== index + 1) throw new Error(`Invalid subtitle block ${index + 1}.`);
    const time = (offset) => {
      const hours = Number(match[offset]), minutes = Number(match[offset + 1]), seconds = Number(match[offset + 2]);
      if (minutes > 59 || seconds > 59) throw new Error('Invalid subtitle timestamp.');
      return hours * 3600 + minutes * 60 + seconds + Number(match[offset + 3]) / 1000;
    };
    const startSeconds = time(2), endSeconds = time(6), content = match[10];
    if (startSeconds < previousEnd - 0.002 || endSeconds <= startSeconds ||
        endSeconds > durationSeconds + 0.002 || !content.trim()) {
      throw new Error(`Subtitle block ${index + 1} has invalid timing or content.`);
    }
    previousEnd = endSeconds;
    return { startSeconds, endSeconds, text: content };
  });
}

export function captionsVTT(captions) {
  const escape = (text) => text.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
  return 'WEBVTT\n\n' + captions.map((cue, index) =>
    `${index + 1}\n${vttTime(cue.startSeconds)} --> ${vttTime(cue.endSeconds)}\n${escape(cue.text)}\n`).join('\n');
}

export function chaptersVTT(chapters, durationSeconds) {
  return 'WEBVTT\n\n' + chapters.map((chapter, index) =>
    `${index + 1}\n${vttTime(chapter.startSeconds)} --> ${vttTime(chapters[index + 1]?.startSeconds ?? durationSeconds)}\n` +
    `${chapter.title}\n`).join('\n');
}

const escapeMarkdown = (text) => text.replace(/[\\`*_[\]<>&|~]/g, '\\$&');

export function transcriptMarkdown(captions, chapters, durationSeconds) {
  return chapters.map((chapter, index) => {
    const end = chapters[index + 1]?.startSeconds ?? durationSeconds;
    const cues = captions.filter((cue) => cue.startSeconds >= chapter.startSeconds && cue.startSeconds < end);
    const paragraphs = [];
    let words = [], start = 0;
    for (const cue of cues) {
      if (!words.length) start = cue.startSeconds;
      words.push(escapeMarkdown(cue.text.replace(/\s+/g, ' ').trim()));
      if (words.join(' ').length >= 450 && /[.!?]$/.test(cue.text.trim())) {
        paragraphs.push(`**${formatTime(start)}** ${words.join(' ')}`);
        words = [];
      }
    }
    if (words.length) paragraphs.push(`**${formatTime(start)}** ${words.join(' ')}`);
    return `### ${formatTime(chapter.startSeconds)} ${escapeMarkdown(chapter.title)}\n\n${paragraphs.join('\n\n')}`;
  }).join('\n\n') + '\n';
}
