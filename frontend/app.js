document.addEventListener('DOMContentLoaded', () => {
    // Mode toggles
    const btnModeSingle = document.getElementById('btn-mode-single');
    const btnModeBatch = document.getElementById('btn-mode-batch');
    const singleFields = document.getElementById('single-fields');
    const batchFields = document.getElementById('batch-fields');
    
    // Inputs
    const cardSecretInput = document.getElementById('card-secret');
    const usernameInput = document.getElementById('username');
    const passwordInput = document.getElementById('password');
    const twoFactorInput = document.getElementById('two-factor');
    const batchDataInput = document.getElementById('batch-data');
    
    // Submit & Query
    const btnSubmit = document.getElementById('btn-submit');
    const queryCardSecretInput = document.getElementById('query-card-secret');
    const btnQuery = document.getElementById('btn-query');
    const queryResults = document.getElementById('query-results');
    const toast = document.getElementById('toast');

    let currentMode = 'single'; // 'single' or 'batch'

    // Fetch system configurations
    async function loadConfig() {
        try {
            const res = await fetch('/api/config');
            const data = await res.json();
            if (res.ok && data.success && data.two_factor_tutorial_url) {
                const tutorialLink = document.getElementById('two-factor-tutorial-link');
                if (tutorialLink) {
                    tutorialLink.href = data.two_factor_tutorial_url;
                }
            }
        } catch (e) {
            console.error('Failed to load system config:', e);
        }
    }
    loadConfig();

    // Mode Switch Handlers
    btnModeSingle.addEventListener('click', () => {
        currentMode = 'single';
        btnModeSingle.classList.add('active');
        btnModeBatch.classList.remove('active');
        singleFields.classList.remove('hidden');
        batchFields.classList.add('hidden');
    });

    btnModeBatch.addEventListener('click', () => {
        currentMode = 'batch';
        btnModeBatch.classList.add('active');
        btnModeSingle.classList.remove('active');
        batchFields.classList.remove('hidden');
        singleFields.classList.add('hidden');
    });

    // Toast function
    function showToast(message, duration = 3000) {
        toast.textContent = message;
        toast.classList.remove('hidden');
        setTimeout(() => {
            toast.classList.add('hidden');
        }, duration);
    }

    // Format backup codes if they are multiple space/dash-separated 8-digit codes
    function formatBackupCodes(twoFactor) {
        if (!twoFactor) return '';
        // Replace dashes and multiple spaces with a single space
        const clean = twoFactor.replace(/[-\s]+/g, ' ').trim();
        const parts = clean.split(' ');
        
        // If all parts are exactly 4 digits, and we have an even number of parts > 1
        const isFourDigitParts = parts.every(p => /^\d{4}$/.test(p));
        if (parts.length > 1 && isFourDigitParts && parts.length % 2 === 0) {
            const formatted = [];
            for (let i = 0; i < parts.length; i += 2) {
                formatted.push(parts[i] + parts[i+1]);
            }
            return formatted.join(',');
        }
        
        // If all parts are exactly 8 digits, and we have multiple parts
        const isEightDigitParts = parts.every(p => /^\d{8}$/.test(p));
        if (parts.length > 1 && isEightDigitParts) {
            return parts.join(',');
        }

        return twoFactor;
    }

    // Validate 2FA key or backup codes format
    function isValid2FA(twoFactor) {
        if (!twoFactor) return false;
        
        // Case 1: 32-character key after removing spaces
        const cleanKey = twoFactor.replace(/\s+/g, '');
        if (/^[a-zA-Z0-9]{32}$/.test(cleanKey)) {
            return true;
        }
        
        // Case 2: 8-digit backup codes (can be multiple, separated by spaces/dashes/commas)
        // Check that the string contains only digits and allowed separators (spaces, dashes, commas)
        const clean = twoFactor.trim();
        if (/^[0-9\s\-,]+$/.test(clean)) {
            const cleanDigits = clean.replace(/\D/g, '');
            if (cleanDigits.length > 0 && cleanDigits.length % 8 === 0) {
                return true;
            }
        }
        
        return false;
    }

    // Check if the Google account email is a personal Google account
    function isPersonalGoogleEmail(email) {
        if (!email) return false;
        const trimmed = email.trim();
        if (!trimmed.includes('@')) {
            return true;
        }
        const lower = trimmed.toLowerCase();
        const personalDomains = [
            'gmail.com', 'googlemail.com',
            'qq.com', 'foxmail.com',
            '163.com', '126.com', 'yeah.net',
            'sina.com', 'sina.cn', 'sohu.com',
            'aliyun.com',
            '139.com', '189.cn', 'wo.cn',
            'outlook.com', 'hotmail.com', 'live.com', 'live.cn', 'msn.com',
            'icloud.com', 'me.com', 'mac.com',
            'yahoo.com', 'ymail.com',
            'proton.me', 'protonmail.com', 'protonmail.ch',
            'aol.com',
            'gmx.com', 'gmx.net', 'mail.com',
            'yandex.com', 'yandex.ru',
            'zoho.com'
        ];
        return personalDomains.some(domain => lower.endsWith('@' + domain) || lower.endsWith('.' + domain));
    }

    // Parse batch line
    function parseBatchLine(line) {
        line = line.trim();
        if (!line) return null;

        let parts = [];
        if (line.includes('----')) {
            parts = line.split('----');
        } else if (line.includes('---')) {
            parts = line.split('---');
        } else if (line.includes('--')) {
            parts = line.split('--');
        } else if (line.includes('|')) {
            parts = line.split('|');
        } else if (line.includes('-')) {
            parts = line.split('-');
        } else {
            parts = [line];
        }

        parts = parts.map(p => p.trim());

        if (parts.length < 3) {
            return {
                username: parts[0] || '',
                password: parts[1] || '',
                two_factor: parts[2] || '',
                error: '格式不正确，至少需要 账号、密码、2FA 3项信息'
            };
        }

        let username = parts[0];
        let password = parts[1];
        let extraEmail = '';
        let twoFactor = '';

        if (parts.length === 3) {
            twoFactor = formatBackupCodes(parts[2]);
        } else {
            extraEmail = parts[2];
            twoFactor = formatBackupCodes(parts[3]);
        }

        if (!isValid2FA(twoFactor)) {
            return {
                username: username,
                password: password,
                two_factor: twoFactor,
                error: '2FA格式不正确，请输入32位密钥或备用验证码'
            };
        }

        if (username.includes('@') && !isPersonalGoogleEmail(username)) {
            return {
                username: username,
                password: password,
                two_factor: twoFactor,
                error: '必须使用Google个人账号，企业组织等账号不可以订阅Google One'
            };
        }

        if (parts.length === 3) {
            return {
                username: username,
                password: password,
                two_factor: twoFactor
            };
        }

        return {
            username: username,
            password: password,
            extra_email: extraEmail,
            two_factor: twoFactor
        };
    }

    // Submit Handler
    btnSubmit.addEventListener('click', async () => {
        const cardSecret = cardSecretInput.value.trim();
        if (!cardSecret) {
            showToast('请输入卡密');
            return;
        }

        let accounts = [];

        if (currentMode === 'single') {
            const username = usernameInput.value.trim();
            const password = passwordInput.value.trim();
            const twoFactor = formatBackupCodes(twoFactorInput.value.trim());

            if (!username || !password || !twoFactor) {
                showToast('请填写所有必填项');
                return;
            }

            if (!isValid2FA(twoFactor)) {
                showToast('2FA格式不正确，请输入32位密钥或备用验证码');
                return;
            }

            if (username.includes('@') && !isPersonalGoogleEmail(username)) {
                showToast('必须使用Google个人账号，企业组织等账号不可以订阅Google One');
                return;
            }

            accounts.push({
                username: username,
                password: password,
                two_factor: twoFactor
            });
        } else {
            const batchText = batchDataInput.value.trim();
            if (!batchText) {
                showToast('请输入批量账号数据');
                return;
            }

            const lines = batchText.split('\n');
            let hasError = false;

            for (let i = 0; i < lines.length; i++) {
                const parsed = parseBatchLine(lines[i]);
                if (parsed) {
                    if (parsed.error) {
                        showToast(`第 ${i + 1} 行: ${parsed.error}`);
                        hasError = true;
                        break;
                    }
                    accounts.push(parsed);
                }
            }

            if (hasError) return;

            if (accounts.length === 0) {
                showToast('未检测到有效的账号数据');
                return;
            }
        }

        // Send submission
        btnSubmit.disabled = true;
        const originalBtnText = btnSubmit.innerHTML;
        btnSubmit.innerHTML = '<span>提交中...</span>';

        try {
            const response = await fetch('/api/submit', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    card_secret: cardSecret,
                    mode: currentMode,
                    accounts: accounts
                })
            });

            const result = await response.json();
            if (response.ok && result.success) {
                showToast(result.message || '提交成功！即将前往查询页面...');
                
                // Clear fields
                if (currentMode === 'single') {
                    usernameInput.value = '';
                    passwordInput.value = '';
                    twoFactorInput.value = '';
                } else {
                    batchDataInput.value = '';
                }

                // Redirect to dedicated query page after 1.5 seconds
                setTimeout(() => {
                    const loader = document.getElementById('page-loader');
                    if (loader) {
                        loader.classList.remove('fade-out');
                    }
                    window.location.href = `/query.html?card_secret=${encodeURIComponent(cardSecret)}`;
                }, 1500);
            } else {
                showToast(result.message || '提交失败，请重试');
            }
        } catch (error) {
            console.error('Submit error:', error);
            showToast('网络请求错误，请稍后重试');
        } finally {
            btnSubmit.disabled = false;
            btnSubmit.innerHTML = originalBtnText;
        }
    });

    // Query Handler - Redirects to dedicated query page
    btnQuery.addEventListener('click', () => {
        const cardSecret = queryCardSecretInput.value.trim();
        if (!cardSecret) {
            showToast('请输入卡密查询状态');
            return;
        }
        const loader = document.getElementById('page-loader');
        if (loader) {
            loader.classList.remove('fade-out');
        }
        setTimeout(() => {
            window.location.href = `/query.html?card_secret=${encodeURIComponent(cardSecret)}`;
        }, 180);
    });

    // Intercept navigation links for smooth transition
    document.querySelectorAll('a.back-link-btn').forEach(link => {
        if (link.getAttribute('href') !== '#') {
            link.addEventListener('click', (e) => {
                e.preventDefault();
                const targetUrl = link.getAttribute('href');
                const loader = document.getElementById('page-loader');
                if (loader) {
                    loader.classList.remove('fade-out');
                }
                setTimeout(() => {
                    window.location.href = targetUrl;
                }, 180);
            });
        }
    });

    // Fade out loader on page load
    const loader = document.getElementById('page-loader');
    if (loader) {
        loader.classList.add('fade-out');
    }

    // Un-hang loader on back button navigation (bfcache recovery)
    window.addEventListener('pageshow', () => {
        const loader = document.getElementById('page-loader');
        if (loader) {
            loader.classList.add('fade-out');
        }
    });

    // Toggle Password Visibility
    const passwordToggleBtn = document.getElementById('password-toggle-btn');
    if (passwordToggleBtn) {
        const eyeIconVisible = passwordToggleBtn.querySelector('.eye-icon-visible');
        const eyeIconHidden = passwordToggleBtn.querySelector('.eye-icon-hidden');
        
        passwordToggleBtn.addEventListener('click', () => {
            if (passwordInput.type === 'password') {
                passwordInput.type = 'text';
                eyeIconVisible.classList.add('hidden');
                eyeIconHidden.classList.remove('hidden');
                passwordToggleBtn.setAttribute('aria-label', '隐藏密码');
            } else {
                passwordInput.type = 'password';
                eyeIconVisible.classList.remove('hidden');
                eyeIconHidden.classList.add('hidden');
                passwordToggleBtn.setAttribute('aria-label', '显示密码');
            }
        });
    }

    // Helper to prevent XSS
    function escapeHtml(str) {
        if (!str) return '';
        return str
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#039;');
    }
});
