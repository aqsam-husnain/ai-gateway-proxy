import { OAuth2Client } from 'google-auth-library';
import http from 'http';
import fs from 'fs';
import path from 'path';
import os from 'os';
import crypto from 'crypto';
import open from 'open';
import { broadcastEvent } from './ui-manager.js';
import { autoLinkProviderConfigs } from './service-manager.js';
import { CONFIG } from './config-manager.js';


async function closeActiveServer(provider, port = null) {
    // 1. Close all previous servers for this provider
    const existing = activeServers.get(provider);
    if (existing) {
        await new Promise((resolve) => {
            existing.server.close(() => {
                activeServers.delete(provider);
                console.log(`[OAuth] Closed old server for provider ${provider} on port ${existing.port}`);
                resolve();
            });
        });
    }

    // 2. If port is specified, check if other providers are using that port
    if (port) {
        for (const [p, info] of activeServers.entries()) {
            if (info.port === port) {
                await new Promise((resolve) => {
                    info.server.close(() => {
                        activeServers.delete(p);
                        console.log(`[OAuth] Closed old server on port ${port} (occupied by provider: ${p})`);
                        resolve();
                    });
                });
            }
        }
    }
}


async function handleGoogleOAuth(providerKey, currentConfig, options = {}) {
    const config = OAUTH_PROVIDERS[providerKey];
    if (!config) {
        throw new Error(`Unknown provider: ${providerKey}`);
    }

    const port = parseInt(options.port) || config.port;
    const externalHost = process.env.OAUTH_EXTERNAL_HOST;
    const redirectUri = externalHost
        ? `${externalHost}:${port}`
        : `http://localhost:${port}`;

    const authClient = new OAuth2Client(config.clientId, config.clientSecret);
    authClient.redirectUri = redirectUri;

    const authUrl = authClient.generateAuthUrl({
        access_type: 'offline',
        prompt: 'select_account',
        scope: config.scope
    });

    // Start callback server
    const credPath = path.join(os.homedir(), config.credentialsDir, config.credentialsFile);

    try {
        await createOAuthCallbackServer(config, redirectUri, authClient, credPath, providerKey, options);
    } catch (error) {
        throw new Error(`Failed to start callback server: ${error.message}`);
    }

    return {
        authUrl,
        authInfo: {
            provider: providerKey,
            redirectUri: redirectUri,
            port: port,
            ...options
        }
    };
}


export async function handleGeminiCliOAuth(currentConfig, options = {}) {
    return handleGoogleOAuth('gemini-cli-oauth', currentConfig, options);
}


export async function handleGeminiAntigravityOAuth(currentConfig, options = {}) {
    return handleGoogleOAuth('gemini-antigravity', currentConfig, options);
}

/**
 * Generate PKCE code verifier
 * @returns {string} Base64URL encoded random string
 */
function generateCodeVerifier() {
    return crypto.randomBytes(32).toString('base64url');
}

/**
 * Generate PKCE code challenge
 * @param {string} codeVerifier - Code verifier
 * @returns {string} Base64URL encoded SHA256 hash
 */
function generateCodeChallenge(codeVerifier) {
    const hash = crypto.createHash('sha256');
    hash.update(codeVerifier);
    return hash.digest('base64url');
}