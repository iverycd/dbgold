create or replace function fun_getpy(p_name in varchar2)
        return varchar2
        as
            v_compare nvarchar2(100);
            v_return nvarchar2(2000);
        function f_nlssort(p_word in varchar2)
        return varchar2
        as
        begin
            return nlssort(p_word, 'nls_sort=schinese_pinyin_m');
        end;
        begin
            for i in 1..length(p_name) loop v_compare := f_nlssort(substr(p_name, i, 1));
                if v_compare >= f_nlssort(' 吖 ') and v_compare <= f_nlssort('驁 ') then
                    v_return := v_return || 'a';
                elsif v_compare >= f_nlssort('八 ') and v_compare <= f_nlssort('簿 ') then
                    v_return := v_return || 'b';
                elsif v_compare >= f_nlssort('嚓 ') and v_compare <= f_nlssort('錯 ') then
                    v_return := v_return || 'c';
                elsif v_compare >= f_nlssort('咑 ') and v_compare <= f_nlssort('鵽 ') then
                    v_return := v_return || 'd';
                elsif v_compare >= f_nlssort('妸 ') and v_compare <= f_nlssort('樲 ') then
                    v_return := v_return || 'e';
                elsif v_compare >= f_nlssort('发 ') and v_compare <= f_nlssort('猤 ') then
                    v_return := v_return || 'f';
                elsif v_compare >= f_nlssort('旮 ') and v_compare <= f_nlssort('腂 ') then
                    v_return := v_return || 'g';
                elsif v_compare >= f_nlssort('妎 ') and v_compare <= f_nlssort('夻 ') then
                    v_return := v_return || 'h';
                elsif v_compare >= f_nlssort('丌 ') and v_compare <= f_nlssort('攈 ') then
                    v_return := v_return || 'j';
                elsif v_compare >= f_nlssort('咔 ') and v_compare <= f_nlssort('穒 ') then
                    v_return := v_return || 'k';
                elsif v_compare >= f_nlssort('垃 ') and v_compare <= f_nlssort('擽 ') then
                    v_return := v_return || 'l';
                elsif v_compare >= f_nlssort('嘸 ') and v_compare <= f_nlssort('椧 ') then
                    v_return := v_return || 'm';
                elsif v_compare >= f_nlssort('拏 ') and v_compare <= f_nlssort('瘧 ') then
                    v_return := v_return || 'n';
                elsif v_compare >= f_nlssort('筽 ') and v_compare <= f_nlssort('漚 ') then
                    v_return := v_return || 'o';
                elsif v_compare >= f_nlssort('妑 ') and v_compare <= f_nlssort('曝 ') then
                    v_return := v_return || 'p';
                elsif v_compare >= f_nlssort('七 ') and v_compare <= f_nlssort('裠') then
                    v_return := v_return || 'q';
                elsif v_compare >= f_nlssort('亽 ') and v_compare <= f_nlssort('鶸 ') then
                    v_return := v_return || 'r';
                elsif v_compare >= f_nlssort('仨 ') and v_compare <= f_nlssort('蜶 ') then
                    v_return := v_return || 's';
                elsif v_compare >= f_nlssort('侤 ') and v_compare <= f_nlssort('籜 ') then
                    v_return := v_return || 't';
                elsif v_compare >= f_nlssort('屲 ') and v_compare <= f_nlssort('鶩 ') then
                    v_return := v_return || 'w';
                elsif v_compare >= f_nlssort('夕 ') and v_compare <= f_nlssort('鑂 ') then
                    v_return := v_return || 'x';
                elsif v_compare >= f_nlssort('丫 ') and v_compare <= f_nlssort('韻 ') then
                    v_return := v_return || 'y';
                elsif v_compare >= f_nlssort('帀') and v_compare <= f_nlssort('咗 ') then
                    v_return := v_return || 'z';
                end if;
            end loop;
            return v_return;
        end;
